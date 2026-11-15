package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/google/uuid"
)

const seedMarker = ".dsh/.skills-seeded"
const legacyOfficeMarker = ".dsh/.officecli-seeded"

// Bootstrap installs the image's initial skills after the runtime has verified
// the Host data directory.
func Bootstrap(ctx context.Context, dataRoot, seedDir string) error {
	dataRoot, err := filepath.EvalSymlinks(dataRoot)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dataRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, directory := range []string{".dsh", ".dsh/skills", ".agents", ".agents/skills"} {
		if err := ensureDir(root, directory); err != nil {
			return err
		}
	}
	marker, err := root.Lstat(seedMarker)
	switch {
	case err == nil && marker.Mode().IsRegular():
		return nil
	case err == nil:
		return errors.New("skill initialization marker must be a regular file")
	case !errors.Is(err, os.ErrNotExist):
		return err
	}

	legacyMarker, err := root.Lstat(legacyOfficeMarker)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	officeAlreadySeeded := err == nil && legacyMarker.Mode().IsRegular()

	registry := Registry{DSHHome: filepath.Join(dataRoot, ".dsh"), AgentsHome: filepath.Join(dataRoot, ".agents")}
	snapshot, err := registry.Snapshot(ctx, Lookup{})
	if err != nil {
		return err
	}
	if !snapshot.Complete {
		return errors.New("cannot check existing user skills")
	}
	installed := make(map[string]Candidate, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		installed[entry.Name] = entry
	}

	seedDir, err = filepath.EvalSymlinks(seedDir)
	if err != nil {
		return err
	}
	seed, err := os.OpenRoot(seedDir)
	if err != nil {
		return err
	}
	defer seed.Close()
	bundles, err := fs.ReadDir(seed.FS(), ".")
	if err != nil {
		return err
	}
	if len(bundles) == 0 {
		return errors.New("Host image contains no seed skills")
	}
	for _, bundle := range bundles {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := bundle.Name()
		if !bundle.IsDir() || !IsName(name) {
			return fmt.Errorf("invalid image skill bundle %q", name)
		}
		// Older data roots seeded OfficeCLI already. Seed only the browser
		// skill so previously deleted OfficeCLI skills stay deleted.
		if officeAlreadySeeded && name != "agent-browser" {
			continue
		}
		loaded, err := load(seed, filepath.Join(name, "SKILL.md"), name)
		if err != nil || loaded.Name != name {
			return fmt.Errorf("invalid image skill %q", name)
		}
		if entry, ok := installed[name]; ok {
			// A previous startup may have stopped after publishing this bundle.
			for _, directory := range []string{filepath.Dir(filepath.Join(entry.root, entry.path)), entry.root} {
				directory, err := filepath.EvalSymlinks(directory)
				if err != nil {
					return err
				}
				relative, err := filepath.Rel(dataRoot, directory)
				if err != nil {
					return err
				}
				if err := syncDir(root, relative); err != nil {
					return err
				}
			}
			continue
		}
		if err := installBundle(ctx, root, seed, name); err != nil {
			return fmt.Errorf("install image skill %q: %w", name, err)
		}
	}
	return publishMarker(root)
}

func installBundle(ctx context.Context, root, seed *os.Root, name string) error {
	source, err := seed.OpenRoot(name)
	if err != nil {
		return err
	}
	defer source.Close()

	stage := ".dsh/.skill-stage-" + uuid.NewString()
	if err := root.Mkdir(stage, 0700); err != nil {
		return err
	}
	defer root.RemoveAll(stage)
	var dirs []string
	err = fs.WalkDir(source.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(stage, filepath.FromSlash(path))
		if entry.IsDir() {
			dirs = append(dirs, target)
			if path == "." {
				return nil
			}
			return root.Mkdir(target, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported skill bundle resource %q", path)
		}
		file, err := source.Open(filepath.FromSlash(path))
		if err != nil {
			return err
		}
		defer file.Close()
		return writeFile(root, target, file, 0600|info.Mode().Perm()&0100)
	})
	if err != nil {
		return err
	}
	for _, path := range slices.Backward(dirs) {
		if err := syncDir(root, path); err != nil {
			return err
		}
	}
	destination := filepath.Join(".dsh/skills", name)
	if _, err := root.Lstat(destination); err == nil {
		// Preserve a user directory whose entry is invalid or has another name.
		destination += "-" + uuid.NewString()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.Rename(stage, destination); err != nil {
		return err
	}
	return errors.Join(syncDir(root, ".dsh/skills"), syncDir(root, ".dsh"))
}

func ensureDir(root *os.Root, path string) error {
	if err := root.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a real directory", path)
	}
	return syncDir(root, filepath.Dir(path))
}

func publishMarker(root *os.Root) error {
	stage := ".dsh/.skills-marker-" + uuid.NewString()
	if err := writeFile(root, stage, nil, 0600); err != nil {
		return err
	}
	defer root.Remove(stage)
	if err := root.Link(stage, seedMarker); err != nil {
		return err
	}
	return syncDir(root, ".dsh")
}

func writeFile(root *os.Root, path string, data io.Reader, mode os.FileMode) error {
	file, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NONBLOCK, mode)
	if err != nil {
		return err
	}
	var writeErr error
	if data != nil {
		_, writeErr = io.Copy(file, data)
	}
	return errors.Join(writeErr, file.Sync(), file.Close())
}

func syncDir(root *os.Root, path string) error {
	directory, err := root.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
