package qas

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Worker struct {
	python  string
	script  string
	config  string
	timeout time.Duration
	logs    *LogBuffer
}

func NewWorker(configPath string, logs *LogBuffer) *Worker {
	python := os.Getenv("PYTHON_PATH")
	if python == "" {
		python = "python3"
	}
	script := os.Getenv("SCRIPT_PATH")
	if script == "" {
		script = "./quark_auto_save.py"
	}
	timeout := 30 * time.Minute
	if value := os.Getenv("TASK_TIMEOUT"); value != "" {
		if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds > 0 {
			timeout = seconds
		}
	}
	return &Worker{python: python, script: script, config: configPath, timeout: timeout, logs: logs}
}

func (w *Worker) command(ctx context.Context, tasks []map[string]any, test bool, cookies []string, pushConfig map[string]any) *exec.Cmd {
	cmd := exec.CommandContext(ctx, w.python, "-u", w.script, w.config)
	env := make([]string, 0, len(os.Environ())+5)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "TASKLIST", "COOKIE", "PUSH_CONFIG", "QUARK_TEST", "PYTHONIOENCODING":
			continue
		default:
			env = append(env, item)
		}
	}
	env = append(env, "PYTHONIOENCODING=utf-8")
	if tasks != nil {
		if data, err := json.Marshal(tasks); err == nil {
			env = append(env, "TASKLIST="+string(data))
		}
	}
	if test {
		if data, err := json.Marshal(cookies); err == nil {
			env = append(env, "COOKIE="+string(data))
		}
		if data, err := json.Marshal(pushConfig); err == nil {
			env = append(env, "PUSH_CONFIG="+string(data))
		}
		env = append(env, "QUARK_TEST=true")
	}
	cmd.Env = env
	return cmd
}

func (w *Worker) Run(ctx context.Context, tasks []map[string]any, test bool, cookies []string, pushConfig map[string]any, onLine func(string)) (int, bool, error) {
	if onLine == nil {
		onLine = func(string) {}
	}
	if w.python == "" || w.script == "" {
		return -1, false, errors.New("Python worker 未配置")
	}
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	cmd := w.command(ctx, tasks, test, cookies, pushConfig)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, false, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return -1, false, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := redactText(strings.TrimRight(scanner.Text(), "\r\n"))
		if line != "" {
			onLine(line)
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if scanErr != nil {
		return -1, timedOut, scanErr
	}
	if waitErr == nil {
		return 0, timedOut, nil
	}
	if exitErr := new(exec.ExitError); errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode(), timedOut, nil
	}
	return -1, timedOut, waitErr
}

type RunRecord struct {
	RunID      string     `json:"run_id"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ReturnCode *int       `json:"returncode"`
	TimedOut   bool       `json:"timed_out"`
	TaskCount  int        `json:"task_count"`
	LogTail    []string   `json:"log_tail"`
	Error      string     `json:"error,omitempty"`
}

type RunManager struct {
	mu         sync.RWMutex
	runs       map[string]*RunRecord
	activeKeys map[string]string
	worker     *Worker
	logs       *LogBuffer
	onComplete func()
}

func NewRunManager(worker *Worker, logs *LogBuffer) *RunManager {
	return &RunManager{runs: map[string]*RunRecord{}, activeKeys: map[string]string{}, worker: worker, logs: logs}
}

func (m *RunManager) SetOnComplete(callback func()) {
	m.mu.Lock()
	m.onComplete = callback
	m.mu.Unlock()
}

func (m *RunManager) Start(ctx context.Context, tasks []map[string]any, fromConfig bool) (map[string]any, error) {
	m.mu.Lock()
	// ponytail: one worker at a time; the Python process rewrites the whole config.json.
	if len(m.activeKeys) > 0 {
		m.mu.Unlock()
		return nil, errors.New("任务与已有运行冲突")
	}
	id := newID()
	keys := []string{"*"}
	record := &RunRecord{RunID: id, Status: "running", StartedAt: time.Now().UTC(), TaskCount: len(tasks), LogTail: []string{}}
	m.runs[id] = record
	m.activeKeys["*"] = id
	m.trimLocked()
	m.mu.Unlock()
	go m.execute(ctx, id, tasks, keys, fromConfig)
	return m.status(id), nil
}

func (m *RunManager) execute(parent context.Context, id string, tasks []map[string]any, keys []string, fromConfig bool) {
	workerTasks := tasks
	if fromConfig {
		// An all-task run must leave TASKLIST unset so the Python worker keeps
		// its normal multi-account sign-in behavior.
		workerTasks = nil
	}
	code, timedOut, err := m.worker.Run(parent, workerTasks, false, nil, nil, func(line string) {
		m.mu.Lock()
		if record := m.runs[id]; record != nil {
			record.LogTail = append(record.LogTail, line)
			if len(record.LogTail) > 200 {
				record.LogTail = append([]string(nil), record.LogTail[len(record.LogTail)-200:]...)
			}
		}
		m.mu.Unlock()
		m.logs.Add("INFO", id, "%s", line)
	})
	m.mu.RLock()
	onComplete := m.onComplete
	m.mu.RUnlock()
	if onComplete != nil {
		onComplete()
	}
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if record := m.runs[id]; record != nil {
		record.ReturnCode = &code
		record.TimedOut = timedOut
		record.FinishedAt = &now
		switch {
		case timedOut:
			record.Status = "timed_out"
		case err != nil:
			record.Status = "failed"
			record.Error = redactText(err)
		case code == 0:
			record.Status = "completed"
		default:
			record.Status = "failed"
		}
	}
	for _, key := range keys {
		if current, ok := m.activeKeys[key]; ok && current == id {
			delete(m.activeKeys, key)
		}
	}
	m.trimLocked()
}

func (m *RunManager) status(id string) map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record := m.runs[id]
	if record == nil {
		return nil
	}
	result := map[string]any{
		"success":     true,
		"run_id":      record.RunID,
		"status":      record.Status,
		"started_at":  record.StartedAt.Format(time.RFC3339Nano),
		"finished_at": nil,
		"returncode":  record.ReturnCode,
		"timed_out":   record.TimedOut,
		"task_count":  record.TaskCount,
		"log_tail":    append([]string(nil), record.LogTail...),
	}
	if record.FinishedAt != nil {
		result["finished_at"] = record.FinishedAt.Format(time.RFC3339Nano)
	}
	if record.Error != "" {
		result["error"] = record.Error
	}
	return result
}

func (m *RunManager) Status(id string) (map[string]any, error) {
	result := m.status(id)
	if result == nil {
		return nil, errors.New("未找到 run_id")
	}
	return result, nil
}

func (m *RunManager) Wait(ctx context.Context, id string) (map[string]any, error) {
	deadline := time.NewTimer(m.worker.timeout + 5*time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := m.Status(id)
		if err != nil {
			return nil, err
		}
		if result["status"] != "running" {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-deadline.C:
			return result, nil
		case <-ticker.C:
		}
	}
}

func (m *RunManager) trimLocked() {
	if len(m.runs) <= 50 {
		return
	}
	for id, record := range m.runs {
		if len(m.runs) <= 50 {
			break
		}
		if record.Status == "running" {
			continue
		}
		delete(m.runs, id)
	}
}

func (m *RunManager) Busy() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.activeKeys) > 0
}

func (m *RunManager) Stream(ctx context.Context, tasks []map[string]any, test bool, cookies []string, pushConfig map[string]any, write func(string) error) error {
	if write == nil {
		return errors.New("stream writer 未配置")
	}
	m.mu.Lock()
	if len(m.activeKeys) > 0 {
		m.mu.Unlock()
		return errors.New("任务与已有运行冲突")
	}
	m.activeKeys["*"] = "stream"
	onComplete := m.onComplete
	m.mu.Unlock()
	defer func() {
		if onComplete != nil {
			onComplete()
		}
		m.mu.Lock()
		if m.activeKeys["*"] == "stream" {
			delete(m.activeKeys, "*")
		}
		m.mu.Unlock()
	}()
	_, _, err := m.worker.Run(ctx, tasks, test, cookies, pushConfig, func(line string) {
		if writeErr := write(line); writeErr != nil {
			// The request context is cancelled on disconnect; retaining the first
			// write error is not useful to the worker and would leak credentials.
			return
		}
		m.logs.Add("INFO", "", "%s", line)
	})
	return err
}

func (m *RunManager) String() string {
	return fmt.Sprintf("runs=%d busy=%t", len(m.runs), m.Busy())
}
