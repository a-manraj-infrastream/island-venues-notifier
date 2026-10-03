// Package delivery handles Eventarc Pub/Sub push deliveries of the
// booking-created event and turns each one into a confirmation e-mail.
//
// The HTTP status code is the acknowledgement protocol with Pub/Sub:
//   - 2xx acknowledges the message (sent, or already sent: a duplicate);
//   - 4xx marks a malformed payload that no retry can fix;
//   - 5xx asks for a redelivery (the e-mail could not be sent).
//
// Note that Pub/Sub push redelivers on any non-2xx answer, 4xx included; only a
// dead-letter policy on the subscription stops a poison message for good. The
// 4xx/5xx split still matters: it is what the logs, metrics and dead-letter
// triage use to tell a bad producer from a broken mail provider.
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/a-manraj-infrastream/island-venues-notifier/internal/mailer"
)

const (
	// Subject of the confirmation e-mail.
	Subject = "Your Island Venues booking request"

	// DefaultDedupeCapacity is how many delivered message ids each instance
	// remembers. At ~100 bytes per id this is about 1 MB.
	DefaultDedupeCapacity = 10000

	// maxBodyBytes bounds a push request. Pub/Sub messages are capped at
	// 10 MB, but a booking event is a few hundred bytes; anything near the cap
	// is not one of ours.
	maxBodyBytes = 64 << 10

	// maxFieldLen bounds every string copied from the event into the e-mail.
	maxFieldLen = 200

	// maxMessageIDLen bounds the ids kept by the dedupe set, so its memory
	// stays at capacity x this, whatever the caller sends. Pub/Sub ids are
	// decimal strings of about 16 digits.
	maxMessageIDLen = 128
)

// Mailer sends one e-mail. Satisfied by mailer.SendGrid and mailer.Log.
type Mailer interface {
	Send(ctx context.Context, msg mailer.Message) error
}

// PushRequest is the body Pub/Sub (and Eventarc's Pub/Sub transport) POSTs to
// a push endpoint.
type PushRequest struct {
	Message struct {
		// Data is base64 in the JSON document; encoding/json decodes it.
		Data       []byte            `json:"data"`
		MessageID  string            `json:"messageId"`
		Attributes map[string]string `json:"attributes,omitempty"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

// BookingCreated is the payload of the booking-created topic, as published by
// island-venues-api. Fields the e-mail does not use are kept as plain strings
// so a formatting change on the producer side (a different timestamp layout,
// say) cannot turn every event into a rejected, never-sent message.
type BookingCreated struct {
	BookingID    string `json:"bookingId"`
	VenueID      string `json:"venueId"`
	VenueName    string `json:"venueName"`
	Date         string `json:"date"`
	Guests       int    `json:"guests"`
	Owner        string `json:"owner"`
	ContactEmail string `json:"contactEmail"`
	CreatedAt    string `json:"createdAt"`
}

// Handler is the push endpoint.
type Handler struct {
	mailer Mailer
	logger *slog.Logger
	seen   *seenSet
}

// NewHandler returns a push handler that sends through m and remembers up to
// dedupeCapacity delivered message ids.
func NewHandler(m Mailer, logger *slog.Logger, dedupeCapacity int) (*Handler, error) {
	if m == nil {
		return nil, errors.New("failed to create delivery handler: mailer is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{mailer: m, logger: logger, seen: newSeenSet(dedupeCapacity)}, nil
}

type response struct {
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, response{Error: "method not allowed"})
		return
	}

	var push PushRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&push); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.reject(ctx, w, http.StatusRequestEntityTooLarge, "", "push body too large")
			return
		}
		h.reject(ctx, w, http.StatusBadRequest, "", fmt.Sprintf("invalid push body: %v", err))
		return
	}

	id := push.Message.MessageID
	if id == "" || len(id) > maxMessageIDLen {
		h.reject(ctx, w, http.StatusBadRequest, "", "message.messageId is required and at most 128 bytes")
		return
	}
	logger := h.logger.With(slog.String("messageId", id), slog.String("subscription", push.Subscription))

	event, err := decodeEvent(push.Message.Data)
	if err != nil {
		h.reject(ctx, w, http.StatusBadRequest, id, err.Error())
		return
	}
	logger = logger.With(slog.String("bookingId", event.BookingID))

	switch h.seen.begin(id) {
	case duplicate:
		logger.InfoContext(ctx, "duplicate delivery acknowledged")
		writeJSON(w, http.StatusOK, response{Status: "duplicate"})
		return
	case inProgress:
		// Acknowledging here would be unsafe: if the in-flight send then
		// fails, its 5xx is ignored because Pub/Sub already has an ack for the
		// message, and the e-mail is lost. Ask for a later redelivery instead.
		logger.WarnContext(ctx, "delivery already in progress, asking for redelivery")
		w.Header().Set("Retry-After", "10")
		writeJSON(w, http.StatusServiceUnavailable, response{Error: "delivery already in progress"})
		return
	}

	err = h.mailer.Send(ctx, ConfirmationEmail(event))
	h.seen.finish(id, err == nil)
	if err != nil {
		logger.ErrorContext(ctx, "failed to send booking confirmation", slog.Any("error", err))
		writeJSON(w, http.StatusBadGateway, response{Error: "failed to send e-mail"})
		return
	}
	logger.InfoContext(ctx, "booking confirmation sent")
	writeJSON(w, http.StatusOK, response{Status: "sent"})
}

func (h *Handler) reject(ctx context.Context, w http.ResponseWriter, status int, id, reason string) {
	h.logger.WarnContext(ctx, "rejected malformed delivery",
		slog.String("messageId", id),
		slog.String("reason", reason),
	)
	writeJSON(w, status, response{Error: reason})
}

// decodeEvent parses and validates the message data. Every rule here guards
// something the e-mail relies on; a failure is permanent (4xx).
func decodeEvent(data []byte) (BookingCreated, error) {
	var ev BookingCreated
	if len(data) == 0 {
		return ev, errors.New("message.data is empty")
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return ev, fmt.Errorf("message.data is not a booking event: %v", err)
	}
	if ev.BookingID == "" || ev.VenueName == "" || ev.Date == "" || ev.ContactEmail == "" {
		return ev, errors.New("booking event requires bookingId, venueName, date and contactEmail")
	}
	for name, v := range map[string]string{
		"bookingId": ev.BookingID, "venueName": ev.VenueName, "date": ev.Date, "contactEmail": ev.ContactEmail,
	} {
		if len(v) > maxFieldLen || strings.ContainsAny(v, "\r\n") {
			return ev, fmt.Errorf("booking event field %s is too long or contains line breaks", name)
		}
	}
	if _, err := time.Parse(time.DateOnly, ev.Date); err != nil {
		return ev, fmt.Errorf("booking event date %q is not YYYY-MM-DD", ev.Date)
	}
	if ev.Guests < 1 {
		return ev, errors.New("booking event guests must be at least 1")
	}
	// Accept a bare address only. A display-name form ("Name <a@b>") or a
	// list would let the producer smuggle extra recipients or header text.
	addr, err := mail.ParseAddress(ev.ContactEmail)
	if err != nil || addr.Address != ev.ContactEmail || addr.Name != "" {
		return ev, errors.New("booking event contactEmail is not a valid e-mail address")
	}
	return ev, nil
}

// ConfirmationEmail renders the plain-text confirmation for a booking request.
func ConfirmationEmail(ev BookingCreated) mailer.Message {
	var b strings.Builder
	b.WriteString("Hello,\n\n")
	b.WriteString("Thank you for your booking request with Island Venues. ")
	b.WriteString("Our venue team will review it and confirm shortly.\n\n")
	b.WriteString("Venue: " + ev.VenueName + "\n")
	b.WriteString("Date: " + ev.Date + "\n")
	b.WriteString("Guests: " + strconv.Itoa(ev.Guests) + "\n")
	b.WriteString("Booking ID: " + ev.BookingID + "\n\n")
	b.WriteString("You can follow or cancel this request under \"My bookings\".\n\n")
	b.WriteString("Island Venues\n")
	return mailer.Message{To: ev.ContactEmail, Subject: Subject, Body: b.String()}
}

func writeJSON(w http.ResponseWriter, status int, body response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
