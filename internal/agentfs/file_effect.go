package agentfs

import (
	"context"
	"errors"
	"os"
)

type FileEffect struct {
	Operation string
	Path      string
	Data      []byte
	Mode      os.FileMode
}

type fileEffectKey struct{}

func WithFileEffects(ctx context.Context, apply func(context.Context, FileEffect) error) context.Context {
	return context.WithValue(ctx, fileEffectKey{}, apply)
}

func applyEffect(ctx context.Context, effect FileEffect) error {
	if apply, ok := ctx.Value(fileEffectKey{}).(func(context.Context, FileEffect) error); ok {
		return apply(ctx, effect)
	}
	return ApplyFileEffect(ctx, effect)
}

// ApplyFileEffect executes the write phase after the caller's observation checks.
func ApplyFileEffect(ctx context.Context, effect FileEffect) error {
	switch effect.Operation {
	case "prepare":
		return ensureContainerParent(effect.Path)
	case "replace":
		return replaceContainerFileContext(ctx, effect.Path, effect.Data, effect.Mode)
	case "create":
		return createContainerFileIfAbsent(ctx, effect.Path, effect.Data)
	default:
		return errors.New("invalid file effect")
	}
}
