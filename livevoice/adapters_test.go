package livevoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/audiox"
	"github.com/NeoLabs-Systems/neolabs-go/wsclient"
)

type adapterScenario struct {
	Protocol string `json:"protocol"`
	Options  struct {
		InputMode string  `json:"inputMode"`
		Thinking  bool    `json:"thinking"`
		APIKey    *string `json:"apiKey"`
		Model     string  `json:"model"`
		Voice     string  `json:"voice"`
	} `json:"options"`
	Connect struct {
		Params struct {
			Instructions string `json:"instructions"`
			History      []struct{ Role, Text string }
			ResumeHandle string `json:"resumeHandle"`
		} `json:"params"`
		OnSetup []any `json:"onSetup"`
		OnStart []any `json:"onStart"`
	} `json:"connect"`
	Steps []struct {
		Server      any `json:"server"`
		ServerClose *struct {
			Code   int
			Reason string
		} `json:"serverClose"`
		Audio  string `json:"audio"`
		Call   string `json:"call"`
		Text   string `json:"text"`
		Handle string `json:"handle"`
		Wait   int    `json:"wait"`
	} `json:"steps"`
	Request      map[string]any `json:"request"`
	ConnectError string         `json:"connectError"`
	Frames       []any          `json:"frames"`
	Events       []any          `json:"events"`
	CloseCodes   []int          `json:"closeCodes"`
}

// canonical round-trips v through JSON so Go values and decoded recordings
// compare equal.
func canonical(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type recorder struct {
	mu     sync.Mutex
	events []any
}

func (r *recorder) add(v ...any) {
	r.mu.Lock()
	r.events = append(r.events, v)
	r.mu.Unlock()
}

// adapters.json holds the Node adapters' frames and events against a scripted
// upstream (gen/adapters.js). The Go adapters must send the same frames and
// report the same events for the same script.
func TestAdaptersMatchNode(t *testing.T) {
	raw, err := os.ReadFile("testdata/adapters.json")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		TaskStarted string            `json:"taskStarted"`
		Scenarios   []adapterScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	silence := audiox.EncodeBase64(audiox.Silence(24000, 100))

	for n, sc := range data.Scenarios {
		var (
			mu        sync.Mutex
			frames    = []any{}
			closes    []int
			request   map[string]any
			conn      *wsclient.Conn
			connected = make(chan struct{}, 1)
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := wsclient.Accept(w, r, wsclient.AcceptOptions{})
			if err != nil {
				return
			}
			mu.Lock()
			conn = c
			auth := any(nil)
			if h := r.Header.Get("Authorization"); h != "" {
				auth = h
			}
			request = map[string]any{"url": r.URL.RequestURI(), "authorization": auth}
			mu.Unlock()
			connected <- struct{}{}
			first := true
			for {
				_, msg, err := c.Read(r.Context())
				if err != nil {
					mu.Lock()
					closes = append(closes, wsclient.CloseStatus(err))
					mu.Unlock()
					return
				}
				var frame map[string]any
				_ = json.Unmarshal(msg, &frame)
				if frame["type"] == "session.input_audio.append" && frame["audio"] == silence {
					continue
				}
				mu.Lock()
				frames = append(frames, frame)
				mu.Unlock()
				if first {
					first = false
					for _, m := range append(sc.Connect.OnSetup, sc.Connect.OnStart...) {
						_ = c.WriteJSON(r.Context(), m)
					}
				}
			}
		}))

		rec := &recorder{events: []any{}}
		var adapter Adapter
		var history []Message
		for _, m := range sc.Connect.Params.History {
			history = append(history, Message{Role: m.Role, Text: m.Text})
		}
		handlers := Handlers{
			OnAudio:            func(pcm []byte) { rec.add("audio", audiox.EncodeBase64(pcm)) },
			OnInputTranscript:  func(s string) { rec.add("inputTranscript", s) },
			OnOutputTranscript: func(s string) { rec.add("outputTranscript", s) },
			OnInterrupted:      func() { rec.add("interrupted") },
			OnTurnComplete:     func(idle bool) { rec.add("turnComplete", idle) },
			OnHangUp:           func() { rec.add("hangUp") },
			OnDelegation: func(handle string) string {
				rec.add("delegation", handle, "")
				return "task-1"
			},
			OnToolCall: func(call ToolCall) (any, bool) {
				if call.Name == "run_task" {
					rec.add("delegation", call.ID, call.Args["request"])
					return map[string]any{"result": data.TaskStarted, "task_id": "task-1"}, true
				}
				rec.add("taskTool", call.Name, call.Args)
				return map[string]any{"tasks": []any{}, "note": "No task is running."}, true
			},
			OnError:  func(err error) { rec.add("error", err.Error()) },
			OnClosed: func(reason, handle string) { rec.add("closed", reason, orNil(handle)) },
		}
		key := "KEY"
		if sc.Options.APIKey != nil {
			key = *sc.Options.APIKey
		}
		model, voice := "m-1", "Kore"
		if sc.Options.Model != "" {
			model, voice = sc.Options.Model, sc.Options.Voice
		}
		switch sc.Protocol {
		case "gemini":
			// The declarations are the app's; the recorded setup carries them.
			adapter = NewGemini(GeminiConfig{
				APIKey: key, BaseURL: srv.URL, Model: model, Voice: voice, InputMode: sc.Options.InputMode,
				Thinking: sc.Options.Thinking, HangUpTool: "end_call", Handlers: handlers,
				FunctionDeclarations: declarationsFrom(t, sc.Frames),
			})
		default:
			adapter = NewOpenAI(OpenAIConfig{APIKey: key, BaseURL: srv.URL, Model: model, Voice: voice, Handlers: handlers})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := adapter.Connect(ctx, ConnectParams{
			Instructions: sc.Connect.Params.Instructions, History: history, ResumeHandle: sc.Connect.Params.ResumeHandle,
		})
		gotError := ""
		if err != nil {
			gotError = err.Error()
		}
		if gotError != sc.ConnectError {
			t.Errorf("scenario %d: connect error %q, Node %q", n, gotError, sc.ConnectError)
		}
		time.Sleep(60 * time.Millisecond)
		for _, step := range sc.Steps {
			switch {
			case step.Server != nil:
				mu.Lock()
				c := conn
				mu.Unlock()
				if s, ok := step.Server.(string); ok {
					_ = c.WriteText(ctx, []byte(s))
				} else {
					_ = c.WriteJSON(ctx, step.Server)
				}
			case step.Audio != "":
				adapter.AppendAudio(audiox.DecodeBase64(step.Audio))
			case step.Call == "startInput":
				adapter.StartInput()
			case step.Call == "endInput":
				adapter.EndInput()
			case step.Call == "say":
				adapter.Say(step.Text, step.Handle)
			case step.Call == "note":
				adapter.Note(step.Text, step.Handle)
			}
			wait := step.Wait
			if wait == 0 {
				wait = 40
			}
			time.Sleep(time.Duration(wait) * time.Millisecond)
		}
		time.Sleep(60 * time.Millisecond)
		adapter.Close()
		if sc.Protocol == "gpt" {
			time.Sleep(1300 * time.Millisecond) // GPT-Live closes the socket a second after its goodbye
		} else {
			time.Sleep(100 * time.Millisecond)
		}
		cancel()
		srv.Close()

		mu.Lock()
		if sc.Request != nil && !reflect.DeepEqual(request, sc.Request) {
			t.Errorf("scenario %d: request %v, Node %v", n, request, sc.Request)
		}
		if !reflect.DeepEqual(canonical(t, frames), canonical(t, orEmpty(sc.Frames))) {
			t.Errorf("scenario %d (%s): frames differ\n go:   %s\n node: %s", n, sc.Protocol, mustJSON(frames), mustJSON(sc.Frames))
		}
		if !reflect.DeepEqual(canonical(t, rec.events), canonical(t, orEmpty(sc.Events))) {
			t.Errorf("scenario %d (%s): events differ\n go:   %s\n node: %s", n, sc.Protocol, mustJSON(rec.events), mustJSON(sc.Events))
		}
		if len(sc.CloseCodes) > 0 && !reflect.DeepEqual(closes, sc.CloseCodes) {
			t.Errorf("scenario %d: close codes %v, Node %v", n, closes, sc.CloseCodes)
		}
		mu.Unlock()
	}
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func declarationsFrom(t *testing.T, frames []any) []any {
	t.Helper()
	if len(frames) == 0 {
		return nil
	}
	setup := frames[0].(map[string]any)["setup"].(map[string]any)
	return setup["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
}
