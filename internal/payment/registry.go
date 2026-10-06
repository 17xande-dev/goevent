package payment

import (
	"errors"
	"fmt"
)

// ErrUnknownGateway is returned by Registry.Lookup for a name no configured
// gateway answers to. It reaches two places: a checkout submitting a gateway the
// store does not offer, and a callback arriving on a route for one.
var ErrUnknownGateway = errors.New("payment: no such gateway")

// Registry is the set of gateways a deployment has configured, in a stable
// order.
//
// A slice rather than a map, because the order is user-visible: it is the order
// the checkout lists payment methods in, and a map would shuffle it between
// restarts. There are two of these at most in practice, so lookup is a loop.
type Registry struct {
	gateways []Gateway
}

// NewRegistry returns a registry over the given gateways, in the order given.
// It refuses an empty set and duplicate names: a store with no way to take money
// should not start, and two gateways answering to one name would make
// /payments/{gateway}/callback ambiguous — which is to say it would credit
// orders using a gateway that never saw them.
func NewRegistry(gateways ...Gateway) (Registry, error) {
	if len(gateways) == 0 {
		return Registry{}, errors.New("payment: no payment gateway is configured")
	}
	seen := make(map[string]bool, len(gateways))
	for _, g := range gateways {
		name := g.Name()
		if name == "" {
			return Registry{}, errors.New("payment: a gateway has no name")
		}
		if seen[name] {
			return Registry{}, fmt.Errorf("payment: two gateways are both named %q", name)
		}
		seen[name] = true
	}
	return Registry{gateways: gateways}, nil
}

// Lookup returns the gateway with this name.
func (r Registry) Lookup(name string) (Gateway, error) {
	for _, g := range r.gateways {
		if g.Name() == name {
			return g, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownGateway, name)
}

// All returns the gateways in order. The checkout renders a chooser from this,
// and shows no chooser at all when there is one.
func (r Registry) All() []Gateway { return r.gateways }

// Len is how many gateways are configured.
func (r Registry) Len() int { return len(r.gateways) }

// Default is the gateway a checkout preselects: the first configured. It panics
// on an empty registry, which NewRegistry does not produce.
func (r Registry) Default() Gateway { return r.gateways[0] }

// CSP collects what every configured gateway needs the Content-Security-Policy
// to permit. Directives nothing asks for come back empty rather than as a list
// of blanks, so a store without a QR gateway gets no extra img-src entry.
func (r Registry) CSP() (formActions, imgSources []string) {
	for _, g := range r.gateways {
		c := g.CSP()
		if c.FormAction != "" {
			formActions = append(formActions, c.FormAction)
		}
		if c.ImgSrc != "" {
			imgSources = append(imgSources, c.ImgSrc)
		}
	}
	return formActions, imgSources
}
