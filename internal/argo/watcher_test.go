package argo

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestErrorMessage(t *testing.T) {
	const invalid = "one or more synchronization tasks are not valid"
	for _, tc := range []struct {
		name   string
		status map[string]any
		want   string
	}{
		{"healthy", map[string]any{"operationState": map[string]any{"phase": "Succeeded", "message": "successfully synced"}}, ""},
		{"first attempt running", map[string]any{"operationState": map[string]any{"phase": "Running", "message": "waiting for healthy state"}}, ""},
		{"retrying", map[string]any{"operationState": map[string]any{
			"phase": "Running", "retryCount": int64(4), "message": invalid + ". Retrying attempt #4 at 12:02PM.",
		}}, invalid + ". Retrying attempt #4 at 12:02PM."},
		{"failed", map[string]any{"operationState": map[string]any{"phase": "Failed", "message": invalid}}, invalid},
		{"warning condition", map[string]any{"conditions": []any{
			map[string]any{"type": "SharedResourceWarning", "message": "Service is part of another app"},
		}}, ""},
		{"error condition", map[string]any{"conditions": []any{
			map[string]any{"type": "ComparisonError", "message": "helm template failed"},
		}}, "helm template failed"},
		{"sync error repeats the operation", map[string]any{
			"conditions":     []any{map[string]any{"type": "SyncError", "message": "Failed sync attempt to abc: " + invalid}},
			"operationState": map[string]any{"phase": "Failed", "message": invalid},
		}, "Failed sync attempt to abc: " + invalid},
		{"different errors", map[string]any{
			"conditions":     []any{map[string]any{"type": "InvalidSpecError", "message": "project apps not found"}},
			"operationState": map[string]any{"phase": "Error", "message": "operation cancelled"},
		}, "project apps not found\noperation cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{"status": tc.status}}
			if got := errorMessage(u); got != tc.want {
				t.Errorf("errorMessage = %q, want %q", got, tc.want)
			}
		})
	}
}
