package delivery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/a-manraj-infrastream/island-venues-notifier/internal/mailer"
)

// fakeMailer records sent messages and fails while err is set. When block is
// non-nil, Send waits on it (to hold a delivery in flight).
type fakeMailer struct {
	mu      sync.Mutex
	sent    []mailer.Message
	err     error
	started chan struct{}
	block   chan struct{}
}

func (f *fakeMailer) Send(_ context.Context, m mailer.Message) error {
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeMailer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func validEvent() map[string]any {
	return map[string]any{
		"bookingId":    "bk_123",
		"venueId":      "v_le_morne",
		"venueName":    "Le Morne Beach Pavilion",
		"date":         "2026-12-12",
		"guests":       80,
		"owner":        "guest@example.com",
		"contactEmail": "guest@example.com",
		"createdAt":    "2026-10-03T08:00:00Z",
	}
}

// pushBody builds an Eventarc Pub/Sub push body around an arbitrary payload.
func pushBody(t *testing.T, id string, payload any) string {
	t.Helper()
	var data []byte
	switch p := payload.(type) {
	case []byte:
		data = p
	default:
		var err error
		if data, err = json.Marshal(p); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"data":        base64.StdEncoding.EncodeToString(data),
			"messageId":   id,
			"publishTime": "2026-10-03T08:00:01Z",
		},
		"subscription": "projects/p/subscriptions/eventarc-booking-created",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestHandler(t *testing.T, m Mailer, capacity int) *Handler {
	t.Helper()
	h, err := NewHandler(m, slog.New(slog.NewJSONHandler(io.Discard, nil)), capacity)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return rec
}

func TestPushSendsConfirmation(t *testing.T) {
	m := &fakeMailer{}
	h := newTestHandler(t, m, 10)

	rec := post(h, pushBody(t, "msg-1", validEvent()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if m.count() != 1 {
		t.Fatalf("sent %d e-mails, want 1", m.count())
	}
	got := m.sent[0]
	if got.To != "guest@example.com" {
		t.Errorf("To = %q", got.To)
	}
	if got.Subject != "Your Island Venues booking request" {
		t.Errorf("Subject = %q", got.Subject)
	}
	for _, want := range []string{
		"Venue: Le Morne Beach Pavilion\n",
		"Date: 2026-12-12\n",
		"Guests: 80\n",
		"Booking ID: bk_123\n",
	} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, got.Body)
		}
	}
}

func TestPushDuplicateIsAcknowledgedWithoutResending(t *testing.T) {
	m := &fakeMailer{}
	h := newTestHandler(t, m, 10)

	body := pushBody(t, "msg-dup", validEvent())
	if rec := post(h, body); rec.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d", rec.Code)
	}
	rec := post(h, body)
	if rec.Code < 200 || rec.Code > 299 {
		t.Fatalf("duplicate status = %d, want 2xx so Pub/Sub stops redelivering", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "duplicate") {
		t.Errorf("duplicate body = %s", rec.Body)
	}
	if m.count() != 1 {
		t.Errorf("sent %d e-mails for one message id, want 1", m.count())
	}

	// A different message id carrying the same booking is a new delivery.
	if rec := post(h, pushBody(t, "msg-other", validEvent())); rec.Code != http.StatusOK {
		t.Fatalf("second message status = %d", rec.Code)
	}
	if m.count() != 2 {
		t.Errorf("sent %d e-mails, want 2", m.count())
	}
}

func TestPushSendFailureIsRetryable(t *testing.T) {
	m := &fakeMailer{err: errors.New("sendgrid down")}
	h := newTestHandler(t, m, 10)
	body := pushBody(t, "msg-retry", validEvent())

	rec := post(h, body)
	if rec.Code < 500 || rec.Code > 599 {
		t.Fatalf("status = %d, want 5xx so Pub/Sub retries", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sendgrid down") {
		t.Errorf("response leaks internal error: %s", rec.Body)
	}

	// The failed attempt must not be remembered as delivered: the retry has
	// to send.
	m.mu.Lock()
	m.err = nil
	m.mu.Unlock()
	if rec := post(h, body); rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d", rec.Code)
	}
	if m.count() != 1 {
		t.Errorf("retry sent %d e-mails, want 1", m.count())
	}
}

func TestPushConcurrentRedeliveryIsNotAcknowledged(t *testing.T) {
	m := &fakeMailer{started: make(chan struct{}, 1), block: make(chan struct{})}
	h := newTestHandler(t, m, 10)
	body := pushBody(t, "msg-race", validEvent())

	first := make(chan int)
	go func() { first <- post(h, body).Code }()
	<-m.started // first delivery is now inside Send

	rec := post(h, body)
	if rec.Code < 500 {
		t.Errorf("in-flight duplicate status = %d, want 5xx (an ack would lose the e-mail if the first send fails)", rec.Code)
	}
	close(m.block)
	if code := <-first; code != http.StatusOK {
		t.Errorf("first delivery status = %d", code)
	}
}

func TestPushMalformedIs4xx(t *testing.T) {
	ev := func(mut func(map[string]any)) map[string]any {
		e := validEvent()
		mut(e)
		return e
	}
	cases := map[string]string{
		"not json":            `{"message":`,
		"no message id":       pushBody(t, "", validEvent()),
		"huge message id":     pushBody(t, strings.Repeat("9", maxMessageIDLen+1), validEvent()),
		"bad base64":          `{"message":{"data":"%%%not-base64","messageId":"m"}}`,
		"empty data":          `{"message":{"data":"","messageId":"m"}}`,
		"data not json":       pushBody(t, "m", []byte("hello")),
		"missing bookingId":   pushBody(t, "m", ev(func(e map[string]any) { delete(e, "bookingId") })),
		"missing email":       pushBody(t, "m", ev(func(e map[string]any) { delete(e, "contactEmail") })),
		"invalid email":       pushBody(t, "m", ev(func(e map[string]any) { e["contactEmail"] = "not-an-address" })),
		"display-name email":  pushBody(t, "m", ev(func(e map[string]any) { e["contactEmail"] = "Eve <eve@example.com>" })),
		"two recipients":      pushBody(t, "m", ev(func(e map[string]any) { e["contactEmail"] = "a@example.com, b@example.com" })),
		"bad date":            pushBody(t, "m", ev(func(e map[string]any) { e["date"] = "12/12/2026" })),
		"zero guests":         pushBody(t, "m", ev(func(e map[string]any) { e["guests"] = 0 })),
		"guests wrong type":   pushBody(t, "m", ev(func(e map[string]any) { e["guests"] = "eighty" })),
		"newline in venue":    pushBody(t, "m", ev(func(e map[string]any) { e["venueName"] = "Pavilion\nBcc: x@example.com" })),
		"oversized venueName": pushBody(t, "m", ev(func(e map[string]any) { e["venueName"] = strings.Repeat("a", 500) })),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			m := &fakeMailer{}
			rec := post(newTestHandler(t, m, 10), body)
			if rec.Code < 400 || rec.Code > 499 {
				t.Errorf("status = %d, want 4xx", rec.Code)
			}
			if m.count() != 0 {
				t.Errorf("malformed delivery sent an e-mail")
			}
			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp["error"] == "" {
				t.Errorf("error body = %s", rec.Body)
			}
		})
	}
}

func TestPushRejectsOversizedBody(t *testing.T) {
	m := &fakeMailer{}
	e := validEvent()
	e["description"] = strings.Repeat("x", maxBodyBytes)
	rec := post(newTestHandler(t, m, 10), pushBody(t, "big", e))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if m.count() != 0 {
		t.Error("oversized delivery sent an e-mail")
	}
}

func TestPushRejectsNonPost(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(t, &fakeMailer{}, 10).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestNewHandlerRequiresMailer(t *testing.T) {
	if _, err := NewHandler(nil, nil, 10); err == nil {
		t.Error("nil mailer accepted")
	}
}
