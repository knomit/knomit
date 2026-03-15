package llm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"
)

func TestTracingAdapter_LogsRequestAndResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockLLMAdapter(ctrl)
	mock.EXPECT().Model().Return("test-model").AnyTimes()
	mock.EXPECT().
		Complete(gomock.Any(), "sys prompt", gomock.Any(), CompletionOptions{ForceJSON: true}, gomock.Any()).
		Return(`{"result":"ok"}`, nil)

	logPath := filepath.Join(t.TempDir(), "trace.log")
	tracer, err := NewTracingAdapter(mock, logPath)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := tracer.Complete(context.Background(), "sys prompt", []Message{{Role: "user", Content: "hello"}}, CompletionOptions{ForceJSON: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tracer.Close()

	if resp != `{"result":"ok"}` {
		t.Fatalf("unexpected response: %s", resp)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)

	for _, want := range []string{"sys prompt", "hello", `\"result\":\"ok\"`, "test-model"} {
		if !strings.Contains(log, want) {
			t.Errorf("trace log missing %q\nlog:\n%s", want, log)
		}
	}
}

func TestTracingAdapter_LogsErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockLLMAdapter(ctrl)
	mock.EXPECT().Model().Return("test-model").AnyTimes()
	mock.EXPECT().
		Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", fmt.Errorf("connection refused"))

	logPath := filepath.Join(t.TempDir(), "trace.log")
	tracer, err := NewTracingAdapter(mock, logPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = tracer.Complete(context.Background(), "sys", nil, CompletionOptions{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	tracer.Close()

	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "connection refused") {
		t.Errorf("trace log missing error\nlog:\n%s", data)
	}
}

func TestTracingAdapter_SequenceNumbers(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockLLMAdapter(ctrl)
	mock.EXPECT().Model().Return("m").AnyTimes()
	mock.EXPECT().
		Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return("ok", nil).Times(2)

	logPath := filepath.Join(t.TempDir(), "trace.log")
	tracer, err := NewTracingAdapter(mock, logPath)
	if err != nil {
		t.Fatal(err)
	}

	tracer.Complete(context.Background(), "s", nil, CompletionOptions{}, nil)
	tracer.Complete(context.Background(), "s", nil, CompletionOptions{}, nil)
	tracer.Close()

	data, _ := os.ReadFile(logPath)
	log := string(data)
	if !strings.Contains(log, `"seq":1`) || !strings.Contains(log, `"seq":2`) {
		t.Errorf("expected seq 1 and 2 in log:\n%s", log)
	}
}
