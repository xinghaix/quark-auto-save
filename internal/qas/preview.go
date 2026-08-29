package qas

import "context"

func (a *App) applySharePreview(_ context.Context, data, task, magic map[string]any, existing []map[string]any) (map[string]any, error) {
	return ApplySharePreview(data, task, magic, existing), nil
}
