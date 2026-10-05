package argo

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestProjectDeleting(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "shop-api"},
		"spec":     map[string]any{"destination": map[string]any{"namespace": "shop"}},
		"status":   map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": "Healthy"}},
	}}
	if s := project(u); s.Deleting || s.Name != "shop-api" || s.Namespace != "shop" {
		t.Errorf("before deletion: %+v", s)
	}
	now := metav1.NewTime(time.Now())
	u.SetDeletionTimestamp(&now)
	if s := project(u); !s.Deleting {
		t.Errorf("being deleted: %+v, want deleting", s)
	}

	// The folder is gone: Argo CD cannot render the app, which is no news.
	u.Object["status"].(map[string]any)["conditions"] = []any{
		map[string]any{"type": "ComparisonError", "message": "open apps/shop-api/values.yaml: no such file or directory"},
	}
	u.Object["status"].(map[string]any)["operationState"] = map[string]any{"phase": "Failed", "message": "values.yaml not found"}
	if s := project(u); s.Message != "" {
		t.Errorf("message while deleting = %q, want none", s.Message)
	}
	u.Object["status"].(map[string]any)["conditions"] = append(u.Object["status"].(map[string]any)["conditions"].([]any),
		map[string]any{"type": "DeletionError", "message": "Deployment shop-api: forbidden"})
	if s := project(u); s.Message != "Deployment shop-api: forbidden" {
		t.Errorf("message while deleting = %q, want the deletion error", s.Message)
	}
}

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
