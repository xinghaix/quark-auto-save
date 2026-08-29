package qas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// applySharePreview keeps the compatibility-sensitive rename algorithm in the
// Python worker while keeping network access and request handling in Go.
func (a *App) applySharePreview(ctx context.Context, data, task, magic map[string]any, existing []map[string]any) (map[string]any, error) {
	if len(task) == 0 {
		return data, nil
	}
	payload, err := json.Marshal(map[string]any{
		"data":        data,
		"task":        task,
		"magic_regex": magic,
		"existing":    existing,
	})
	if err != nil {
		return data, err
	}
	helper := envOr("PREVIEW_SCRIPT_PATH", "./app/runtime/preview.py")
	python := "python3"
	if a.worker != nil && a.worker.python != "" {
		python = a.worker.python
	}
	previewCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(previewCtx, python, "-u", helper)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if previewCtx.Err() != nil {
			return data, fmt.Errorf("重命名预览超时")
		}
		return data, fmt.Errorf("重命名预览失败: %w", err)
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return data, fmt.Errorf("重命名预览返回无效 JSON: %w", err)
	}
	return result, nil
}
