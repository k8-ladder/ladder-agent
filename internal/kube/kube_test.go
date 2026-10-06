package kube

import (
	"errors"
	"testing"
)

func TestLogsError(t *testing.T) {
	unknown := errors.New("the server rejected our request for an unknown reason (get pods web-1)")
	status := []byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"previous terminated container \"app\" in pod \"web-1\" not found","reason":"BadRequest","code":400}`)
	if got := LogsError(unknown, status); got.Error() != `previous terminated container "app" in pod "web-1" not found` {
		t.Errorf("from a Status: %v", got)
	}
	if got := LogsError(unknown, []byte("not json")); got != unknown {
		t.Errorf("from another body: %v", got)
	}
	if got := LogsError(nil, status); got != nil {
		t.Errorf("without an error: %v", got)
	}
}
