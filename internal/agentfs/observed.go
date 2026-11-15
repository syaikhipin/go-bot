package agentfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

var ErrNotObserved = errors.New("existing file must be read before it can be changed")

type fileObservation struct {
	version string
	present bool
}

// ObservedContainerView is one Session's in-memory read-before-mutate policy.
// Like DeepSeek's fs-observation-policy, a cold Session starts with no observed
// files; the v4 event log does not persist these ephemeral file versions.
type ObservedContainerView struct {
	ContainerView
	mu       sync.Mutex
	observed map[string]fileObservation
}

func NewObservedContainerView(view ContainerView) *ObservedContainerView {
	return &ObservedContainerView{ContainerView: view, observed: make(map[string]fileObservation)}
}

func (v *ObservedContainerView) ReadFile(path string, maxBytes int64) (ReadResult, error) {
	result, err := v.ReadFileAt(path, 0, maxBytes)
	if err != nil {
		return ReadResult{}, err
	}
	if err := v.RecordReadObservation(result); err != nil {
		return ReadResult{}, err
	}
	return result, nil
}

func (v *ObservedContainerView) ReadFileAt(path string, offset, maxBytes int64) (ReadResult, error) {
	if maxBytes <= 0 {
		return ReadResult{}, errors.New("positive read limit required")
	}
	path, err := v.path(path)
	if err != nil {
		return ReadResult{}, err
	}
	file, err := openContainerExisting(path, os.O_RDONLY, containerRegular)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A missing file is an authoritative absence only for the target
			// resolved at this instant. Missing parents need no entry: unseen
			// targets already have the same create-only write intent.
			if target, _, targetErr := containerMutationTarget(path); targetErr == nil {
				v.mu.Lock()
				v.observed[target] = fileObservation{}
				v.mu.Unlock()
			}
		}
		return ReadResult{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return ReadResult{}, err
	}
	result, err := readBoundedFileAt(file, offset, maxBytes)
	if err != nil {
		return ReadResult{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return ReadResult{}, err
	}
	if fileVersion(before) != fileVersion(after) {
		return ReadResult{}, ErrChanged
	}
	target, _, err := containerMutationTarget(path)
	if err != nil {
		return ReadResult{}, err
	}
	targetInfo, err := os.Stat(target)
	if err != nil || !os.SameFile(before, targetInfo) || fileVersion(targetInfo) != fileVersion(before) {
		return ReadResult{}, ErrChanged
	}
	result.observedTarget = target
	result.observedVersion = fileVersion(before)
	result.ResolvedPath = target
	return result, nil
}

// The file tool calls this only after it has accepted the UTF-8 read result.
// A concurrent replacement between the read and this decision fails closed.
func (v *ObservedContainerView) RecordReadObservation(result ReadResult) error {
	if result.observedTarget == "" || result.observedVersion == "" {
		return errors.New("read result has no authoritative file observation")
	}
	info, err := os.Stat(result.observedTarget)
	if err != nil || fileVersion(info) != result.observedVersion {
		return ErrChanged
	}
	v.mu.Lock()
	v.observed[result.observedTarget] = fileObservation{version: result.observedVersion, present: true}
	v.mu.Unlock()
	return nil
}

func (v *ObservedContainerView) WriteFile(path string, data []byte) (string, error) {
	return v.WriteFileContext(context.Background(), path, data)
}

func (v *ObservedContainerView) WriteFileContext(ctx context.Context, path string, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := v.path(path)
	if err != nil {
		return "", err
	}
	if err := applyEffect(ctx, FileEffect{Operation: "prepare", Path: path}); err != nil {
		return "", uncertainMutation(fmt.Errorf("create file parent: %w", err))
	}
	target, _, err := containerMutationTarget(path)
	if err != nil {
		return "", err
	}
	lock := containerMutationLock(target)
	lock.Lock()
	defer lock.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	currentTarget, mode, err := containerMutationTarget(path)
	if err != nil || currentTarget != target {
		return "", ErrChanged
	}
	prior, observed := v.observed[target]
	info, err := os.Stat(target)
	switch {
	case err == nil:
		if !observed || !prior.present {
			return "", ErrNotObserved
		}
		if fileVersion(info) != prior.version {
			return "", ErrChanged
		}
		if err := applyEffect(ctx, FileEffect{Operation: "replace", Path: target, Data: data, Mode: mode}); err != nil {
			return "", err
		}
	case errors.Is(err, os.ErrNotExist):
		if observed && prior.present {
			return "", ErrChanged
		}
		if err := applyEffect(ctx, FileEffect{Operation: "create", Path: target, Data: data}); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	if err := v.observePublished(target); err != nil {
		return "", uncertainMutation(err)
	}
	return digest(data), nil
}

func (v *ObservedContainerView) EditFile(path, oldText, newText, expectedSHA256 string, replaceAll bool, maxBytes int64) (string, error) {
	return v.EditFileContext(context.Background(), path, oldText, newText, expectedSHA256, replaceAll, maxBytes)
}

func (v *ObservedContainerView) EditFileContext(ctx context.Context, path, oldText, newText, expectedSHA256 string, replaceAll bool, maxBytes int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if maxBytes <= 0 {
		return "", errors.New("positive edit limit required")
	}
	path, err := v.path(path)
	if err != nil {
		return "", err
	}
	target, _, err := containerMutationTarget(path)
	if err != nil {
		return "", err
	}
	lock := containerMutationLock(target)
	lock.Lock()
	defer lock.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	prior, observed := v.observed[target]
	if !observed {
		return "", ErrNotObserved
	}
	if !prior.present {
		return "", os.ErrNotExist
	}
	file, err := openContainerExisting(path, os.O_RDONLY, containerRegular)
	if err != nil {
		return "", ErrChanged
	}
	info, statErr := file.Stat()
	if statErr != nil || fileVersion(info) != prior.version {
		_ = file.Close()
		return "", ErrChanged
	}
	current, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return "", err
	}
	if int64(len(current)) > maxBytes {
		return "", errors.New("file exceeds edit limit")
	}
	if expectedSHA256 != "" && digest(current) != expectedSHA256 {
		return "", ErrChanged
	}
	replacement, err := applyBoundedLiteralEdit(current, oldText, newText, replaceAll, maxBytes)
	if err != nil {
		return "", err
	}
	currentTarget, mode, err := containerMutationTarget(path)
	if err != nil || currentTarget != target {
		return "", ErrChanged
	}
	currentInfo, err := os.Stat(target)
	if err != nil || fileVersion(currentInfo) != prior.version {
		return "", ErrChanged
	}
	if err := applyEffect(ctx, FileEffect{Operation: "replace", Path: target, Data: replacement, Mode: mode}); err != nil {
		return "", err
	}
	if err := v.observePublished(target); err != nil {
		return "", uncertainMutation(err)
	}
	return digest(replacement), nil
}

// Called only with v.mu held after publication. A failed stat leaves the
// mutation's outcome uncertain even though the publish operation returned.
func (v *ObservedContainerView) observePublished(target string) error {
	info, err := os.Stat(target)
	if err != nil {
		delete(v.observed, target)
		return err
	}
	v.observed[target] = fileObservation{version: fileVersion(info), present: true}
	return nil
}

// The hard link is the no-replace publication primitive. A separate exists
// check followed by rename would be able to clobber an external creator.
func createContainerFileIfAbsent(ctx context.Context, target string, data []byte) error {
	return createContainerFileIfAbsentWithLink(ctx, target, data, os.Link)
}

func createContainerFileIfAbsentWithLink(ctx context.Context, target string, data []byte, link func(string, string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Dir(target)
	tempPath, cleanup, err := stagePrivateContainerFile(parent, target, ".agent-create-", data, 0o600)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := link(tempPath, target); err != nil {
		// A failed NFS response can arrive after the server has linked our
		// staging inode. Distinguish that uncertain case from another file's
		// collision instead of reporting a clean no-mutation rejection.
		if published, inspectErr := os.Lstat(target); inspectErr == nil {
			staged, stageErr := os.Lstat(tempPath)
			if stageErr == nil && os.SameFile(staged, published) {
				return uncertainMutation(err)
			}
			return ErrNotObserved
		} else if !errors.Is(inspectErr, os.ErrNotExist) {
			return uncertainMutation(errors.Join(err, inspectErr))
		}
		if errors.Is(err, os.ErrExist) {
			return ErrNotObserved
		}
		return uncertainMutation(err)
	}
	dir, err := os.Open(parent)
	if err != nil {
		return uncertainMutation(err)
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return uncertainMutation(err)
	}
	return nil
}
