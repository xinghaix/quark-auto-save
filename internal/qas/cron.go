package qas

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cronField struct {
	values map[int]bool
	any    bool
}

type cronSchedule struct {
	fields [5]cronField
}

func parseCron(expr string) (*cronSchedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, errors.New("crontab 必须包含 5 个字段")
	}
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	result := &cronSchedule{}
	for index, part := range parts {
		field, err := parseCronField(part, ranges[index][0], ranges[index][1], index == 4)
		if err != nil {
			return nil, fmt.Errorf("crontab 第 %d 个字段无效: %w", index+1, err)
		}
		result.fields[index] = field
	}
	return result, nil
}

func parseCronField(value string, minimum, maximum int, sundayAlias bool) (cronField, error) {
	result := cronField{values: map[int]bool{}}
	for _, piece := range strings.Split(value, ",") {
		if piece == "" {
			return result, errors.New("空字段")
		}
		base := piece
		step := 1
		if strings.Contains(piece, "/") {
			parts := strings.Split(piece, "/")
			if len(parts) != 2 {
				return result, errors.New("步长格式无效")
			}
			base = parts[0]
			parsed, err := strconv.Atoi(parts[1])
			if err != nil || parsed < 1 {
				return result, errors.New("步长无效")
			}
			step = parsed
		}
		start, end := minimum, maximum
		switch {
		case base == "*" || base == "":
		case strings.Contains(base, "-"):
			parts := strings.Split(base, "-")
			if len(parts) != 2 {
				return result, errors.New("范围格式无效")
			}
			var err error
			start, err = strconv.Atoi(parts[0])
			if err != nil {
				return result, errors.New("范围起点无效")
			}
			end, err = strconv.Atoi(parts[1])
			if err != nil {
				return result, errors.New("范围终点无效")
			}
		default:
			parsed, err := strconv.Atoi(base)
			if err != nil {
				return result, errors.New("数值无效")
			}
			start, end = parsed, parsed
		}
		if start < minimum || end > maximum || start > end {
			return result, errors.New("数值超出范围")
		}
		for current := start; current <= end; current += step {
			if sundayAlias && current == 7 {
				result.values[0] = true
			} else {
				result.values[current] = true
			}
		}
	}
	result.any = (value == "*" || strings.HasPrefix(value, "*/"))
	return result, nil
}

func (c *cronSchedule) matches(now time.Time) bool {
	values := [5]int{now.Minute(), now.Hour(), now.Day(), int(now.Month()), int(now.Weekday())}
	for index := 0; index < 5; index++ {
		if index == 2 || index == 4 {
			continue
		}
		if !c.fields[index].values[values[index]] {
			return false
		}
	}
	domMatch := c.fields[2].values[values[2]]
	dowMatch := c.fields[4].values[values[4]]
	if c.fields[2].any || c.fields[4].any {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}

type Scheduler struct {
	mu       sync.RWMutex
	expr     string
	state    string
	schedule *cronSchedule
	cancel   chan struct{}
	done     chan struct{}
	run      func()
}

func NewScheduler(run func()) *Scheduler {
	return &Scheduler{state: "stopped", run: run}
}

func (s *Scheduler) Reload(expr string) error {
	if strings.TrimSpace(expr) == "" {
		// Compatibility: the legacy scheduler returned false for an empty
		// crontab without removing an already registered job.
		return nil
	}
	var schedule *cronSchedule
	var err error
	schedule, err = parseCron(expr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	oldCancel, oldDone := s.cancel, s.done
	if oldCancel != nil {
		close(oldCancel)
	}
	s.expr = strings.TrimSpace(expr)
	s.schedule = schedule
	if schedule == nil {
		s.state, s.cancel, s.done = "stopped", nil, nil
		s.mu.Unlock()
		if oldDone != nil {
			<-oldDone
		}
		return nil
	}
	cancel := make(chan struct{})
	done := make(chan struct{})
	s.cancel, s.done, s.state = cancel, done, "running"
	s.mu.Unlock()
	if oldDone != nil {
		<-oldDone
	}
	go s.loop(schedule, cancel, done)
	return nil
}

func (s *Scheduler) loop(schedule *cronSchedule, cancel <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var lastMinute time.Time
	for {
		select {
		case <-cancel:
			return
		case now := <-ticker.C:
			minute := now.Truncate(time.Minute)
			if minute.Equal(lastMinute) || !schedule.matches(now) {
				continue
			}
			lastMinute = minute
			if s.run != nil {
				go s.run()
			}
		}
	}
}

func (s *Scheduler) State() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *Scheduler) Expression() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expr
}

func (s *Scheduler) Shutdown() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done, s.state = nil, nil, "stopped"
	s.mu.Unlock()
	if cancel != nil {
		close(cancel)
	}
	if done != nil {
		<-done
	}
}
