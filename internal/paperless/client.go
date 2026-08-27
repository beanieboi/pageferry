package paperless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type Task struct {
	ID         string
	Status     string
	DocumentID int
	Result     string
}

func New(baseURL, token string, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("paperless_url must be an absolute http(s) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("paperless_url scheme must be http or https")
	}
	if token == "" {
		return nil, errors.New("Paperless token is empty")
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Transport: transport, Timeout: timeout}}, nil
}

func (c *Client) Upload(ctx context.Context, path, filename string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open queued PDF: %w", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return "", err
	}

	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	if _, err := writer.CreateFormFile("document", filepath.Base(filename)); err != nil {
		return "", err
	}
	suffix := []byte("\r\n--" + writer.Boundary() + "--\r\n")
	body := io.MultiReader(bytes.NewReader(prefix.Bytes()), file, bytes.NewReader(suffix))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/documents/post_document/", body)
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(prefix.Len()) + stat.Size() + int64(len(suffix))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload to Paperless: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read Paperless upload response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Paperless upload returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	taskID, err := parseTaskID(data)
	if err != nil {
		return "", err
	}
	return taskID, nil
}

func (c *Client) LookupTask(ctx context.Context, taskID string) (Task, bool, error) {
	endpoint := c.baseURL + "/api/tasks/?task_id=" + url.QueryEscape(taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Task{}, false, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return Task{}, false, fmt.Errorf("query Paperless task: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Task{}, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return Task{}, false, fmt.Errorf("Paperless task query returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	tasks, err := parseTasks(data)
	if err != nil {
		return Task{}, false, err
	}
	for _, task := range tasks {
		if task.ID == taskID {
			return task, true, nil
		}
	}
	if len(tasks) == 1 && tasks[0].ID == "" {
		tasks[0].ID = taskID
		return tasks[0], true, nil
	}
	return Task{}, false, nil
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")
}

func parseTaskID(data []byte) (string, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return "", fmt.Errorf("decode Paperless task id: %w", err)
	}
	var id string
	switch typed := value.(type) {
	case string:
		id = typed
	case map[string]any:
		for _, key := range []string{"task_id", "id"} {
			if candidate, ok := typed[key].(string); ok {
				id = candidate
				break
			}
		}
	}
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 200 {
		return "", fmt.Errorf("Paperless returned an invalid task id: %s", strings.TrimSpace(string(data)))
	}
	return id, nil
}

func parseTasks(data []byte) ([]Task, error) {
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode Paperless tasks: %w", err)
	}
	var rows []any
	switch typed := raw.(type) {
	case []any:
		rows = typed
	case map[string]any:
		if results, ok := typed["results"].([]any); ok {
			rows = results
		} else {
			rows = []any{typed}
		}
	default:
		return nil, errors.New("Paperless tasks response has an unexpected shape")
	}
	tasks := make([]Task, 0, len(rows))
	for _, row := range rows {
		object, ok := row.(map[string]any)
		if !ok {
			continue
		}
		task := Task{ID: stringValue(object["task_id"]), Status: strings.ToUpper(stringValue(object["status"])), Result: stringValue(object["result"])}
		if task.Result == "" {
			task.Result = stringValue(object["result_data"])
		}
		task.DocumentID = documentID(object)
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func documentID(object map[string]any) int {
	if result, ok := object["result_data"].(map[string]any); ok {
		for _, key := range []string{"document_id", "duplicate_of"} {
			if id := intValue(result[key]); id > 0 {
				return id
			}
		}
	}
	for _, key := range []string{"related_document", "document_id"} {
		if id := intValue(object[key]); id > 0 {
			return id
		}
	}
	result := stringValue(object["result"])
	match := regexp.MustCompile(`(?i)(?:document id|duplicate of)\s+(\d+)`).FindStringSubmatch(result)
	if len(match) == 2 {
		id, _ := strconv.Atoi(match[1])
		return id
	}
	return 0
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case json.Number:
		id, _ := strconv.Atoi(typed.String())
		return id
	case float64:
		return int(typed)
	case string:
		id, _ := strconv.Atoi(typed)
		return id
	default:
		return 0
	}
}
