package payment

import "errors"

// ErrRetryable means verification could not finish. It is not evidence that the
// notification is invalid; the callback endpoint must let the provider retry.
var ErrRetryable = errors.New("payment: verification temporarily unavailable")
