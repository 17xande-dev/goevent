package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/17xande-dev/password"
)

// HashPassword hashes a nonempty password for an administrator account.
func HashPassword(ctx context.Context, pw string) (string, error) {
	if pw == "" {
		return "", errors.New("auth: password must not be empty")
	}
	return password.Hash(ctx, pw)
}

// ErrPasswordFormat means a stored hash is not in a format the app can verify.
var ErrPasswordFormat = errors.New("auth: unrecognised password hash format")

func validatePasswordHash(hash string) error {
	if err := password.Validate(hash); err != nil {
		return fmt.Errorf("%w: %w (generate one with `make hashpw`)", ErrPasswordFormat, err)
	}
	return nil
}
