package snapscan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/17xande-dev/goevent/internal/payment"
)

// maxAPIResponseBytes caps the confirmation response. A payment object is a few
// hundred bytes; this is room for a large one and not for anything else.
const maxAPIResponseBytes = 64 << 10

// Payment is SnapScan's payment object, as much of it as this server reads.
//
// Amounts are integer cents. RequiredAmount is a pointer because it is absent
// unless the QR carried an amount, and "absent" has to be distinguishable from
// "zero" — the difference is between a payment against somebody else's QR and a
// free one.
type Payment struct {
	ID                int64  `json:"id"`
	Status            string `json:"status"`
	TotalAmount       int64  `json:"totalAmount"`
	TipAmount         int64  `json:"tipAmount"`
	RequiredAmount    *int64 `json:"requiredAmount"`
	SnapCode          string `json:"snapCode"`
	SnapCodeReference string `json:"snapCodeReference"`
	MerchantReference string `json:"merchantReference"`
	AuthCode          string `json:"authCode"`
	TransactionType   string `json:"transactionType"`
}

// getPayment reads one payment back from SnapScan over an authenticated
// connection. This is the half of ParseCallback that is actually believed.
//
// Authentication is HTTP Basic with the API key as the username and no password,
// which is what SnapScan's documentation specifies.
func (g *Gateway) getPayment(ctx context.Context, id int64) (Payment, error) {
	endpoint := g.apiBase + "/payments/" + strconv.FormatInt(id, 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Payment{}, fmt.Errorf("snapscan: build confirmation request: %w", err)
	}
	req.SetBasicAuth(g.cfg.APIKey, "")
	req.Header.Set("Accept", "application/json")

	res, err := g.client.Do(req)
	if err != nil {
		// A network failure is not a rejection: the notification may well be
		// genuine. It is still not confirmed, so the payment stays as it was and
		// SnapScan's retry — three minutes of them — gets another chance.
		return Payment{}, fmt.Errorf("%w: %w: %w", payment.ErrRetryable, ErrNotValidated, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxAPIResponseBytes))
	if err != nil {
		return Payment{}, fmt.Errorf("%w: %w: read response: %w", payment.ErrRetryable, ErrNotValidated, err)
	}
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// A signed notification naming a payment SnapScan has never heard of.
		// Worth the distinct message: it means the signing key is shared with
		// something that is not SnapScan.
		return Payment{}, fmt.Errorf("%w: SnapScan has no payment %d", ErrNotValidated, id)
	case http.StatusUnauthorized:
		return Payment{}, fmt.Errorf("%w: %w: SNAPSCAN_API_KEY was refused", payment.ErrRetryable, ErrNotValidated)
	default:
		return Payment{}, fmt.Errorf("%w: %w: SnapScan answered %d", payment.ErrRetryable, ErrNotValidated, res.StatusCode)
	}

	var p Payment
	if err := json.Unmarshal(body, &p); err != nil {
		return Payment{}, fmt.Errorf("%w: %w: response is not a payment object: %v", payment.ErrRetryable, ErrNotValidated, err)
	}
	if p.ID != id {
		// Belt and braces against a caching proxy or a redirect handing back
		// somebody else's payment.
		return Payment{}, fmt.Errorf("%w: asked for payment %d and got %d", ErrNotValidated, id, p.ID)
	}
	// A refund arriving down the payment webhook would otherwise read as a
	// payment and credit a registration for money going the other way.
	if p.TransactionType != "" && p.TransactionType != "payment" {
		return Payment{}, fmt.Errorf("%w: payment %d is a %q, not a payment", ErrNotValidated, id, p.TransactionType)
	}
	return p, nil
}
