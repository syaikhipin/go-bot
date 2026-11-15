//go:build !linux

package executor

import "errors"

func VerifyHostRuntime(string) error {
	return errors.New("user Host requires Linux mount namespaces")
}
