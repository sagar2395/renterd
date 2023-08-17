package alerts

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/renterd/webhooks"
	"go.uber.org/zap"
)

func TestWebhooks(t *testing.T) {
	mgr := webhooks.NewManager(zap.NewNop().Sugar())
	alerts := NewManager(mgr)

	mux := http.NewServeMux()
	var events []webhooks.Action
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		var event webhooks.Action
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// register a hook
	wh := webhooks.Webhook{
		Module: webhookModule,
		URL:    fmt.Sprintf("http://%v/events", srv.Listener.Addr().String()),
	}
	if hookID := wh.String(); hookID != fmt.Sprintf("%v.%v.%v", wh.URL, wh.Module, "") {
		t.Fatalf("wrong result for wh.String(): %v != %v", wh.String(), hookID)
	}
	err := mgr.Register(wh)
	if err != nil {
		t.Fatal(err)
	}

	// perform some actions that should trigger the endpoint
	a := Alert{
		ID:        types.Hash256{1},
		Severity:  SeverityWarning,
		Timestamp: time.Unix(0, 0),
	}
	alerts.Register(a)
	alerts.Dismiss(types.Hash256{1})

	// list hooks
	hooks := mgr.List()
	if len(hooks) != 1 {
		t.Fatal("wrong number of hooks")
	} else if hooks[0].URL != wh.URL {
		t.Fatal("wrong hook id")
	} else if hooks[0].Event != wh.Event {
		t.Fatal("wrong event", hooks[0].Event)
	} else if hooks[0].Module != wh.Module {
		t.Fatal("wrong module", hooks[0].Module)
	}

	// unregister hook
	if !mgr.Delete(hooks[0]) {
		t.Fatal("hook not deleted")
	}

	// perform an action that should not trigger the endpoint
	alerts.Register(Alert{
		ID:        types.Hash256{2},
		Severity:  SeverityWarning,
		Timestamp: time.Now(),
	})

	// check events
	if len(events) != 3 {
		t.Fatal("wrong number of hits", len(events))
	}
	assertEvent := func(event webhooks.Action, module, id string, hasPayload bool) {
		t.Helper()
		if event.Module != module {
			t.Fatal("wrong event module", event.Module, module)
		} else if event.ID != id {
			t.Fatal("wrong event id", event.ID, id)
		} else if hasPayload && event.Payload == nil {
			t.Fatal("missing payload")
		}
	}
	assertEvent(events[0], "", webhooks.WebhookEventPing, false)
	assertEvent(events[1], webhookModule, webhookEventRegister, true)
	assertEvent(events[2], webhookModule, webhookEventDismiss, true)
}
