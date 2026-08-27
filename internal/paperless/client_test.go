package paperless

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUploadAndPaginatedTaskLookup(t *testing.T) {
	const token = "secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token "+token {
			t.Error("missing authorization")
		}
		switch r.URL.Path {
		case "/api/documents/post_document/":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				http.Error(w, "bad", 400)
				return
			}
			file, _, err := r.FormFile("document")
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != "PDF DATA" {
				t.Errorf("body = %q", data)
			}
			io.WriteString(w, `"task-123"`)
		case "/api/tasks/":
			if r.URL.Query().Get("task_id") != "task-123" {
				t.Error("wrong task id")
			}
			io.WriteString(w, `{"count":1,"results":[{"task_id":"task-123","status":"SUCCESS","result_data":{"document_id":42}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(path, []byte("PDF DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := New(server.URL, token, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.Upload(context.Background(), path, "scan.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if id != "task-123" {
		t.Fatalf("id = %q", id)
	}
	task, found, err := client.LookupTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !found || task.Status != "SUCCESS" || task.DocumentID != 42 {
		t.Fatalf("task = %#v, found=%v", task, found)
	}
}

func TestLegacyTaskArray(t *testing.T) {
	tasks, err := parseTasks([]byte(`[{"task_id":"abc","status":"FAILURE","result":"duplicate of 99"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].DocumentID != 99 {
		t.Fatalf("tasks = %#v", tasks)
	}
}
