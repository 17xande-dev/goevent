package payment

import (
	"errors"
	"testing"
)

func TestRegistry_LooksUpByName(t *testing.T) {
	a := NewFakeNamed("a", "A")
	b := NewFakeNamed("b", "B")

	r, err := NewRegistry(a, b)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if r.Len() != 2 {
		t.Errorf("Len = %d, want 2", r.Len())
	}
	// The order is user-visible — it is the order the checkout lists payment
	// methods in — so it is the order given, not whatever a map would have
	// produced.
	if r.All()[0].Name() != "a" || r.All()[1].Name() != "b" {
		t.Errorf("All = %q, %q, want the configured order", r.All()[0].Name(), r.All()[1].Name())
	}
	if r.Default().Name() != "a" {
		t.Errorf("Default = %q, want the first configured", r.Default().Name())
	}

	got, err := r.Lookup("b")
	if err != nil || got.Name() != "b" {
		t.Errorf("Lookup(b) = %v, %v", got, err)
	}
	if _, err := r.Lookup("c"); !errors.Is(err, ErrUnknownGateway) {
		t.Errorf("Lookup(c) = %v, want %v", err, ErrUnknownGateway)
	}
	// An empty name must not resolve to anything. The callback route and the
	// checkout form both pass user-controlled strings through here.
	if _, err := r.Lookup(""); !errors.Is(err, ErrUnknownGateway) {
		t.Errorf("Lookup(empty) = %v, want %v", err, ErrUnknownGateway)
	}
}

// Two gateways under one name would make /payments/{gateway}/callback ambiguous,
// which is to say it would settle orders using a provider that never saw them.
func TestNewRegistry_Refusals(t *testing.T) {
	if _, err := NewRegistry(); err == nil {
		t.Error("NewRegistry accepted no gateways at all")
	}
	if _, err := NewRegistry(NewFakeNamed("same", "One"), NewFakeNamed("same", "Two")); err == nil {
		t.Error("NewRegistry accepted two gateways with the same name")
	}
	if _, err := NewRegistry(NewFakeNamed("", "Nameless")); err == nil {
		t.Error("NewRegistry accepted a gateway with no name")
	}
}

// The CSP is assembled from what each gateway declares. A directive nothing asks
// for must come back empty rather than as a list of blanks — an empty source in a
// policy is not harmless, it is a parse error the browser resolves by ignoring
// the directive.
func TestRegistry_CSPCollectsOnlyWhatIsAskedFor(t *testing.T) {
	form := NewFakeNamed("form", "Form post") // declares a form-action origin
	qr := NewFakeNamed("qr", "QR")
	qr.Kind = HandoverLink // declares an img-src origin instead

	r, err := NewRegistry(form, qr)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	forms, images := r.CSP()
	if len(forms) != 1 || forms[0] != "https://gateway.example" {
		t.Errorf("form-action = %q", forms)
	}
	if len(images) != 1 || images[0] != "https://gateway.example" {
		t.Errorf("img-src = %q", images)
	}

	// A store with only a form-post gateway must add nothing to img-src.
	only, err := NewRegistry(form)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, images := only.CSP(); len(images) != 0 {
		t.Errorf("img-src = %q for a gateway that loads no images", images)
	}
}

func TestCallback_PaidIsOnlyTrueForPaid(t *testing.T) {
	for outcome, want := range map[Outcome]bool{
		OutcomePaid:      true,
		OutcomePending:   false,
		OutcomeFailed:    false,
		OutcomeCancelled: false,
	} {
		if got := (Callback{Outcome: outcome}).Paid(); got != want {
			t.Errorf("Outcome %v: Paid = %v, want %v", outcome, got, want)
		}
	}
	// The zero value must not read as paid. A gateway that forgets to set an
	// outcome should fail closed.
	if (Callback{}).Paid() {
		t.Error("a zero Callback reads as paid")
	}
}
