package carrier

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrForeignLayerDir is returned when a layer root exists, holds
// content, and carries no carrier marker: the root is not a
// carrier layer, and Materialize refuses to wipe or build over
// work the carrier did not create (Review Focus).
var ErrForeignLayerDir = errors.New("carrier: layer root exists and is not a carrier layer")

// The layer root's layout: layer.json is the marker, view is the
// writable directory, and a build of the view happens in a
// staging sibling (view.building-*) renamed into place.
const (
	markerName    = "layer.json"
	viewName      = "view"
	stagingPrefix = "view.building-"
)

// layerMarker is the marker file's content ({base_id, driver},
// canonical JSON): the carrier's proof that it created the layer
// root, and the identity a re-materialization is checked against.
type layerMarker struct {
	BaseID string   `json:"base_id"`
	Driver DriverID `json:"driver"`
}

// A Layer is one materialized workspace: Dir is the directory a
// run writes in. Bookkeeping never lives inside Dir (a marker in
// the writable tree would pollute the run's own delta).
type Layer struct {
	Dir    string
	BaseID string
	Driver DriverID

	root string // the layer root Remove deletes in full
}

// Materialize builds (or reuses) the layer at layerRoot from
// baseDir, whose content identity is baseID (Decision 8(b): the
// snapshot workspace tree ID). Layout: layerRoot/layer.json is
// the marker ({base_id, driver}, canonical JSON); layerRoot/view
// is the writable directory (Layer.Dir).
//
// The marker law: a root carrying a marker whose base_id and
// driver match the request is returned as-is (idempotent
// re-materialization — a second handle on the same layer,
// contents preserved). A marker naming a different base or
// driver means the base changed: the root is wiped and rebuilt,
// never merged (AC-F5-3). A marker that fails to decode is a
// descriptive error, never a guess and never a wipe. A non-empty
// root with no marker is refused with ErrForeignLayerDir — the
// wipe deletes only what the carrier created (Review Focus). The
// single exception is a driver's own crash remnant, recognized
// by that driver's exact remnant layout and recovered by that
// driver (Task 3's overlay remnant law); any markerless shape a
// driver cannot recognize as its own stays refused, unwiped.
//
// Builds go to a staging sibling (view.building-*) renamed into
// place, with the marker written last; a staging remnant from a
// crashed build is removed by the next Materialize of the same
// root.
func Materialize(sel Selection, baseDir, baseID, layerRoot string) (*Layer, error) {
	drv, ok := driverByID(sel.Driver)
	if !ok {
		return nil, fmt.Errorf("carrier: selection names unknown driver %q", sel.Driver)
	}
	baseInfo, err := os.Stat(baseDir)
	if err != nil {
		return nil, fmt.Errorf("carrier: base %q: %w", baseDir, err)
	}
	if !baseInfo.IsDir() {
		return nil, fmt.Errorf("carrier: base %q: not a directory", baseDir)
	}

	preExisted := false
	var oldMarker *layerMarker
	if info, err := os.Stat(layerRoot); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("carrier: layer root %q is not a directory: %w", layerRoot, ErrForeignLayerDir)
		}
		preExisted = true
		marker, found, markerErr := readMarker(layerRoot)
		if markerErr != nil {
			return nil, markerErr
		}
		if found {
			m := marker
			oldMarker = &m
		}
		if found && marker.BaseID == baseID && marker.Driver == sel.Driver {
			view := filepath.Join(layerRoot, viewName)
			if _, err := os.Stat(view); err == nil {
				// Idempotent re-materialization: the layer is
				// returned as-is; only staging remnants of a
				// crashed later build are cleaned.
				if err := removeStagingRemnants(layerRoot); err != nil {
					return nil, err
				}
				return &Layer{Dir: view, BaseID: baseID, Driver: sel.Driver, root: layerRoot}, nil
			}
			// A matching marker whose view is gone is a layer
			// the carrier owns but cannot hand out: fall
			// through and rebuild it.
		}
		// The markerless-content check: a root with content
		// but no marker is foreign — with the one exception
		// the marker law names. The selected driver is asked
		// first whether the root is exactly its own crash
		// remnant (Task 3's overlay remnant law); a recognized
		// remnant is recovered by the driver and the build
		// proceeds. Anything the driver does not recognize
		// stays subject to the foreign law, unwiped.
		if !found {
			recovered := false
			if rr, ok := drv.(remnantRecoverer); ok {
				var rerr error
				recovered, rerr = rr.recoverRemnant(layerRoot)
				if rerr != nil {
					return nil, rerr
				}
			}
			if !recovered {
				if err := refuseForeignContent(layerRoot); err != nil {
					return nil, err
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("carrier: stat layer root %q: %w", layerRoot, err)
	}

	// Build (or rebuild) the layer in a staging sibling.
	if err := os.MkdirAll(layerRoot, 0o755); err != nil {
		return nil, fmt.Errorf("carrier: create layer root %q: %w", layerRoot, err)
	}
	if err := removeStagingRemnants(layerRoot); err != nil {
		return nil, err
	}
	// A driver that manages the view itself — a mountpoint
	// cannot be built in a staging sibling and renamed into
	// place — builds in the root directly.
	if vd, ok := drv.(viewDriver); ok {
		return buildViewLayer(vd, sel, baseDir, baseID, layerRoot, preExisted, oldMarker)
	}
	staging, err := os.MkdirTemp(layerRoot, stagingPrefix)
	if err != nil {
		return nil, fmt.Errorf("carrier: create staging view in %q: %w", layerRoot, err)
	}
	if err := drv.materialize(baseDir, staging); err != nil {
		os.RemoveAll(staging)
		if !preExisted {
			// A failed first build leaves no layer root
			// behind; a failed rebuild leaves the previous
			// layer intact.
			os.RemoveAll(layerRoot)
		}
		return nil, err
	}
	if err := os.Chmod(staging, baseInfo.Mode().Perm()); err != nil {
		os.RemoveAll(staging)
		return nil, fmt.Errorf("carrier: set view mode from base %q: %w", baseDir, err)
	}

	// The build succeeded: the root's previous contents (an old
	// view, an old marker, driver state) are wiped — never
	// merged — and the staging view is renamed into place. The
	// previous layer's driver state is released first: a
	// view-managing driver's view may be a live mount, and
	// deleting through a mount corrupts the layer's backing
	// store instead of removing the layer.
	if oldMarker != nil {
		if err := releaseLayerState(*oldMarker, layerRoot); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(layerRoot)
	if err != nil {
		return nil, fmt.Errorf("carrier: list layer root %q: %w", layerRoot, err)
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(staging) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(layerRoot, e.Name())); err != nil {
			return nil, fmt.Errorf("carrier: wipe previous layer content %q: %w", e.Name(), err)
		}
	}
	view := filepath.Join(layerRoot, viewName)
	if err := os.Rename(staging, view); err != nil {
		return nil, fmt.Errorf("carrier: rename staging view into %q: %w", layerRoot, err)
	}

	// The marker is written last: a root with a view but no
	// marker is a build that never completed.
	if err := writeMarker(layerRoot, layerMarker{BaseID: baseID, Driver: sel.Driver}); err != nil {
		return nil, err
	}
	return &Layer{Dir: view, BaseID: baseID, Driver: sel.Driver, root: layerRoot}, nil
}

// buildViewLayer builds a layer for a viewDriver: the view is
// created at its final path in the layer root and the driver
// populates it there, keeping its state as the view's siblings;
// the marker is written last, exactly as in the staged path.
// Because the build occupies the root itself, the previous
// layer's state is released and its contents wiped before the
// build — unlike the staged path, a failed rebuild does not
// leave the previous layer standing. The marker law still
// governs handout: a root whose view is gone is rebuilt, never
// returned as-is, so a wiped root can never be handed out as a
// stale layer.
func buildViewLayer(drv viewDriver, sel Selection, baseDir, baseID, layerRoot string, preExisted bool, oldMarker *layerMarker) (*Layer, error) {
	if oldMarker != nil {
		if err := releaseLayerState(*oldMarker, layerRoot); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(layerRoot)
	if err != nil {
		return nil, fmt.Errorf("carrier: list layer root %q: %w", layerRoot, err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(layerRoot, e.Name())); err != nil {
			return nil, fmt.Errorf("carrier: wipe previous layer content %q: %w", e.Name(), err)
		}
	}
	view := filepath.Join(layerRoot, viewName)
	if err := drv.materializeView(baseDir, layerRoot, view); err != nil {
		if !preExisted {
			// A failed first build leaves no layer root
			// behind (the staged path's law).
			os.RemoveAll(layerRoot)
		}
		return nil, err
	}
	if err := writeMarker(layerRoot, layerMarker{BaseID: baseID, Driver: sel.Driver}); err != nil {
		// The build succeeded but cannot be recorded: release
		// the driver's state so no mount outlives the error,
		// and leave no root behind on a first build.
		drv.remove(&Layer{Driver: sel.Driver, root: layerRoot})
		if !preExisted {
			os.RemoveAll(layerRoot)
		}
		return nil, err
	}
	return &Layer{Dir: view, BaseID: baseID, Driver: sel.Driver, root: layerRoot}, nil
}

// releaseLayerState releases the driver state of the previous
// layer at a root before its contents are wiped. Only a
// view-managing driver (viewDriver) holds state a wipe cannot
// safely delete through — a live mount at the view; other
// drivers' state is plain files (copy) or nonexistent (the
// not-yet-implemented stubs never completed a build, so no
// marker can name one from a real layer).
func releaseLayerState(marker layerMarker, root string) error {
	drv, ok := driverByID(marker.Driver)
	if !ok {
		return nil
	}
	if _, ok := drv.(viewDriver); !ok {
		return nil
	}
	if err := drv.remove(&Layer{Driver: marker.Driver, root: root}); err != nil {
		return fmt.Errorf("carrier: release previous %s layer at %q: %w", marker.Driver, root, err)
	}
	return nil
}

// Remove tears the layer down and deletes layerRoot entirely.
func (l *Layer) Remove() error {
	if l == nil || l.root == "" {
		return errors.New("carrier: remove a layer with no root")
	}
	if drv, ok := driverByID(l.Driver); ok {
		if err := drv.remove(l); err != nil {
			return fmt.Errorf("carrier: tear down %s layer at %q: %w", l.Driver, l.root, err)
		}
	}
	if err := os.RemoveAll(l.root); err != nil {
		return fmt.Errorf("carrier: delete layer root %q: %w", l.root, err)
	}
	return nil
}

// readMarker reads the layer root's marker. found is false when
// no marker file exists. A marker that exists but does not
// decode is an error — never a guess and never a wipe.
func readMarker(root string) (marker layerMarker, found bool, err error) {
	data, err := os.ReadFile(filepath.Join(root, markerName))
	if errors.Is(err, os.ErrNotExist) {
		return layerMarker{}, false, nil
	}
	if err != nil {
		return layerMarker{}, false, fmt.Errorf("carrier: read layer marker in %q: %w", root, err)
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return layerMarker{}, true, fmt.Errorf("carrier: layer marker in %q does not decode: %w", root, err)
	}
	return marker, true, nil
}

// writeMarker writes the marker file (canonical JSON: the struct
// marshals in field order, one line, no extra keys).
func writeMarker(root string, marker layerMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("carrier: encode layer marker: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, markerName), data, 0o644); err != nil {
		return fmt.Errorf("carrier: write layer marker in %q: %w", root, err)
	}
	return nil
}

// refuseForeignContent enforces the foreign-directory law for a
// markerless root: staging remnants of the carrier's own crashed
// builds are recognizable and pass (the build removes them); any
// other content makes the root foreign, and it is refused
// untouched.
func refuseForeignContent(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("carrier: list layer root %q: %w", root, err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), stagingPrefix) {
			continue
		}
		return fmt.Errorf("carrier: layer root %q holds %q, which the carrier did not create: %w", root, e.Name(), ErrForeignLayerDir)
	}
	return nil
}

// removeStagingRemnants deletes staging views left by crashed
// builds of the same root.
func removeStagingRemnants(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("carrier: list layer root %q: %w", root, err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), stagingPrefix) {
			if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
				return fmt.Errorf("carrier: remove staging remnant %q: %w", e.Name(), err)
			}
		}
	}
	return nil
}
