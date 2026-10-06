package handler

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/17xande-dev/goevent/internal/middleware"
	"github.com/17xande-dev/goevent/internal/payment"
	"github.com/17xande-dev/goevent/internal/registrations"
)

// maxCallbackBytes caps what this endpoint will read. A gateway's notification is
// well under a kilobyte, and this route is unauthenticated until the gateway has
// vouched for the body — so the limit is applied while reading, not after.
const maxCallbackBytes = 64 << 10

// The gateway callback is the highest-stakes surface in the system: it is
// unauthenticated by definition — a payment provider cannot be given a session or
// a CSRF token — and it is the only thing that can decide money has changed hands.
// Everything about it follows from those two facts.
//
//   - It is registered outside the CSRF group, by being mounted on the server's
//     own mux rather than the first-party one. A route cannot drift out of an
//     exemption it was never inside.
//   - The gateway authenticates the notification; this handler does not try to.
//     ParseCallback returning without an error is the proof, and there is no code
//     path here that acts on an unproven one.
//   - Permanent rejections and completed transactions answer 200. Temporary
//     verification or persistence failures answer 503 so the provider retries.
//   - What only this side can do, it does — in registrations.MarkPaid: find the
//     payment, check it was made with this gateway and for this amount, and keep
//     a replay from confirming twice.

// RegisterPayments wires the gateway callback. It takes its own mux registration
// on purpose — see the comment above.
func (h *Handler) RegisterPayments(mux *http.ServeMux) {
	// Rate limited, and this is the surface the limiter exists for: unauthenticated,
	// and every accepted request makes the server POST to the gateway to validate
	// it, which is an amplifier. A throttled request has not been processed: 429
	// with Retry-After lets the provider try again.
	mux.Handle("POST /payments/{gateway}/callback", h.limits.callback(http.HandlerFunc(h.paymentCallback)))
}

func (h *Handler) paymentCallback(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	defer func() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "30")
		}
		w.WriteHeader(status)
	}()

	// The {gateway} segment names which provider is claiming to have taken money,
	// and only a configured one is listened to.
	name := r.PathValue("gateway")
	gateway, err := h.gateways.Lookup(name)
	if err != nil {
		h.log.Warn("payment callback for an unknown gateway", "gateway", name, "remote", r.RemoteAddr)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxCallbackBytes+1))
	if err != nil {
		h.log.Error("payment callback: read body", "error", err)
		status = http.StatusServiceUnavailable
		return
	}

	sourceIP := middleware.ClientIP(r, h.cfg.ClientIPSource)
	cb, err := gateway.ParseCallback(r.Context(), payment.Notification{
		Body: body, Header: r.Header, SourceIP: sourceIP,
	})
	if err != nil {
		if errors.Is(err, payment.ErrRetryable) {
			status = http.StatusServiceUnavailable
		}
		// Which check failed is the whole diagnostic value of this line: a
		// signature mismatch is usually a passphrase that disagrees with the
		// dashboard, an IP rejection a proxy or a changed range.
		h.log.Warn("rejected payment callback",
			"gateway", gateway.Name(), "source_ip", sourceIP, "error", err, "body_bytes", len(body))
		return
	}

	if err := h.applyCallback(r, gateway, cb); err != nil {
		status = http.StatusServiceUnavailable
	}
}

// applyCallback is everything that happens once a notification is proven
// genuine. It returns an error only for a failure worth the gateway retrying.
func (h *Handler) applyCallback(r *http.Request, gateway payment.Gateway, cb payment.Callback) error {
	log := h.log.With("gateway", gateway.Name(), "payment", cb.PaymentID,
		"gateway_ref", cb.Ref, "gateway_status", cb.Status)
	n := registrations.Notice{
		PaymentID: cb.PaymentID, Gateway: gateway.Name(), Ref: cb.Ref, Status: cb.Status,
		AmountCents: cb.AmountCents, Amount: cb.Amount, Raw: string(cb.Raw),
	}

	if !cb.Paid() {
		// Cancelled, failed, or still pending at the gateway. Recorded, never
		// acted on, and never allowed to contradict a payment that succeeded.
		err := h.regs.RecordUnpaid(r.Context(), n, unpaidStatus(cb.Outcome))
		return h.callbackResult(log, err, "payment did not complete")
	}

	outcome, err := h.regs.MarkPaid(r.Context(), n, h.onConfirm())
	switch {
	case err != nil:
		return h.callbackResult(log, err, "")
	case outcome == registrations.AlreadyPaid:
		// Routine: gateways retry, and this is what stops a retry confirming twice.
		log.Info("ignored a replayed payment notification")
	case outcome == registrations.Confirmed:
		log.Info("payment received; registration confirmed")
	default:
		log.Info("payment received; registration not yet paid in full")
	}
	return nil
}

// callbackResult turns a store error into the callback's answer: the ones that
// are about the notification are logged and acknowledged, since no retry will
// change them; anything else is a fault worth a retry.
func (h *Handler) callbackResult(log *slog.Logger, err error, ok string) error {
	switch {
	case err == nil:
		if ok != "" {
			log.Info(ok)
		}
		return nil
	case errors.Is(err, registrations.ErrNotFound):
		// A genuine notification for a payment this server never made. Most
		// likely two deployments sharing one merchant account.
		log.Warn("payment callback names an unknown payment")
		return nil
	case errors.Is(err, registrations.ErrWrongGateway):
		// A gateway only ever proves things about its own account, so one
		// provider's notification must not settle another's payment.
		log.Warn("payment callback names a payment made through a different gateway")
		return nil
	case errors.Is(err, registrations.ErrAmountMismatch):
		// The figure paid is not the figure asked for, so nothing is confirmed.
		// Recorded for whoever reconciles it.
		log.Error("payment amount does not match; NOT confirming the registration")
		return nil
	default:
		// The money may be taken and this server failed to record it. The
		// gateway's retry is the recovery, and the operation is idempotent.
		log.Error("failed to apply a payment notification", "error", err)
		return err
	}
}

// unpaidStatus maps a normalised outcome onto a payment status. An outcome this
// code cannot place stays pending — not knowing a payment failed is not the
// same as knowing it did.
func unpaidStatus(o payment.Outcome) string {
	switch o {
	case payment.OutcomeCancelled:
		return "cancelled"
	case payment.OutcomeFailed:
		return "failed"
	default:
		return "pending"
	}
}
