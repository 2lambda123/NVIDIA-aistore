// Package fs provides mountpath and FQN abstractions and methods to resolve/map stored content
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package fs

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/jsp"
)

const vmdCopies = 3

type (
	fsMpathMD struct {
		Mountpath string `json:"mountpath"`
		Enabled   bool   `json:"enabled"`

		Fs     string   `json:"fs"`
		FsType string   `json:"fs_type"`
		FsID   cos.FsID `json:"fs_id"`

		Ext interface{} `json:"ext,omitempty"` // Reserved for future extension.
	}

	// Short for VolumeMetaData.
	VMD struct {
		Version    uint64                `json:"version,string"` // Version which tracks any updates happened to Mountpaths.
		Mountpaths map[string]*fsMpathMD `json:"mountpaths"`     // mountpath => metadata
		DaemonID   string                `json:"daemon_id"`      // ID of the daemon to which the mountpaths belong(ed).
		cksum      *cos.Cksum            // Checksum of loaded VMD.
	}

	// errors
	StorageIntegrityError struct {
		msg  string
		code int
	}
)

// interface guard
var _ jsp.Opts = (*VMD)(nil)

func (*VMD) JspOpts() jsp.Options { return jsp.CCSign(cmn.MetaverVMD) }

func newVMD(expectedSize int) *VMD {
	return &VMD{
		Mountpaths: make(map[string]*fsMpathMD, expectedSize),
	}
}

func (vmd *VMD) load(mpath string) (err error) {
	fpath := filepath.Join(mpath, cmn.VmdFname)
	if vmd.cksum, err = jsp.LoadMeta(fpath, vmd); err != nil {
		return err
	}
	if vmd.DaemonID == "" {
		debug.Assert(false) // Cannot happen in normal environment.
		return fmt.Errorf("daemon id is empty for vmd on %q", mpath)
	}
	return nil
}

func (vmd *VMD) persist() (err error) {
	cnt, availCnt := PersistOnMpaths(cmn.VmdFname, "", vmd, vmdCopies, nil, nil /*wto*/)
	if cnt > 0 {
		return
	}
	if availCnt == 0 {
		glog.Errorf("cannot store VMD: %v", ErrNoMountpaths)
		return
	}
	return fmt.Errorf("failed to store VMD on any of the mountpaths (%d)", availCnt)
}

func (vmd *VMD) equal(other *VMD) bool {
	debug.Assert(vmd.cksum != nil)
	debug.Assert(other.cksum != nil)
	return vmd.DaemonID == other.DaemonID &&
		vmd.Version == other.Version &&
		vmd.cksum.Equal(other.cksum)
}

func (vmd *VMD) String() string { return string(cos.MustMarshal(vmd)) }

func CreateNewVMD(daemonID string) (vmd *VMD, err error) {
	var (
		curVersion          uint64
		available, disabled = Get()
	)
	// Try to load the currently stored vmd to determine the version.
	vmd, err = LoadVMD(available)
	if err != nil {
		glog.Warning(err) // TODO: handle
		err = nil
	}
	if vmd != nil {
		curVersion = vmd.Version
	}

	vmd = newVMD(len(available))
	vmd.DaemonID = daemonID
	vmd.Version = curVersion + 1 // Bump the version.

	addMountpath := func(mpath *MountpathInfo, enabled bool) {
		vmd.Mountpaths[mpath.Path] = &fsMpathMD{
			Mountpath: mpath.Path,
			Enabled:   enabled,

			Fs:     mpath.Fs,
			FsType: mpath.FsType,
			FsID:   mpath.FsID,
		}
	}

	for _, mpath := range available {
		addMountpath(mpath, true /*enabled*/)
	}
	for _, mpath := range disabled {
		addMountpath(mpath, false /*enabled*/)
	}
	err = vmd.persist()
	return
}

// initVMD and LoadVMD loads VMD from given paths (aside: no templates, etc.):
// - Returns nil if VMD does not exist
// - Returns error on failure to validate or load existing VMD
func initVMD(fspaths cos.StringSet) (*VMD, error) {
	available := make(MPI, len(fspaths)) // strictly to satisfy LoadVMD (below)
	for mpath := range fspaths {
		available[mpath] = nil
	}
	return LoadVMD(available)
}

func LoadVMD(available MPI) (vmd *VMD, err error) {
	l := len(available)
	for mpath := range available {
		var v *VMD
		v, err = _loadVMD(vmd, mpath, l)
		if err != nil {
			return
		}
		if v != nil {
			vmd = v
		}
	}
	return vmd, nil
}

func _loadVMD(vmd *VMD, mpath string, l int) (*VMD, error) {
	var (
		v   = newVMD(l)
		err = v.load(mpath)
	)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		err = newVMDLoadErr(mpath, err)
		return nil, err
	}
	if vmd == nil {
		return v, nil
	}
	if v.DaemonID != vmd.DaemonID {
		return nil, newMpathIDMismatchErr(v.DaemonID, vmd.DaemonID, mpath)
	}
	if v.Version > vmd.Version { // NOTE: take the newer
		glog.Warningf("%s (on %q) version greater than %s", v, mpath, vmd)
		return v, nil
	}
	if v.Version < vmd.Version { // NOTE: ignore the older
		glog.Warningf("%s (on %q) version lesser than %s", v, mpath, vmd)
	} else if !v.equal(vmd) { // NOTE: must be identical
		err = newVMDMismatchErr(vmd, v, mpath)
	}
	return nil, err
}

// LoadDaemonID loads the daemon ID present as xattr on given mount paths.
func LoadDaemonID(mpaths cos.StringSet) (mDaeID string, err error) {
	for mp := range mpaths {
		daeID, err := loadDaemonIDXattr(mp)
		if err != nil {
			return "", err
		}
		if daeID == "" {
			continue
		}
		if mDaeID != "" {
			if mDaeID != daeID {
				return "", newMpathIDMismatchErr(mDaeID, daeID, mp)
			}
			continue
		}
		mDaeID = daeID
	}
	return
}

func loadDaemonIDXattr(mpath string) (daeID string, err error) {
	b, err := GetXattr(mpath, daemonIDXattr)
	if err == nil {
		daeID = string(b)
		return
	}
	if cmn.IsErrXattrNotFound(err) {
		err = nil
	}
	return
}

////////////
// errors //
////////////

func (sie *StorageIntegrityError) Error() string {
	return fmt.Sprintf("[%s]: %s", siError(sie.code), sie.msg)
}

func newMpathIDMismatchErr(mainDaeID, tid, mpath string) *StorageIntegrityError {
	return &StorageIntegrityError{
		code: siMpathIDMismatch,
		msg:  fmt.Sprintf("target ID mismatch: %q vs %q (%q)", mainDaeID, tid, mpath),
	}
}

func newVMDIDMismatchErr(vmd *VMD, tid string) *StorageIntegrityError {
	return &StorageIntegrityError{
		code: siTargetIDMismatch,
		msg:  fmt.Sprintf("%s has a different target ID: %q != %q", vmd, vmd.DaemonID, tid),
	}
}

func newVMDMissingMpathErr(mpath string) *StorageIntegrityError {
	return &StorageIntegrityError{
		code: siMpathMissing,
		msg:  fmt.Sprintf("mountpath %q not in VMD", mpath),
	}
}

func newConfigMissingMpathErr(mpath string) *StorageIntegrityError {
	return &StorageIntegrityError{
		code: siMpathMissing,
		msg:  fmt.Sprintf("mountpath %q in VMD but not in the config", mpath),
	}
}

func newVMDLoadErr(mpath string, err error) *StorageIntegrityError {
	return &StorageIntegrityError{
		code: siMetaCorrupted,
		msg:  fmt.Sprintf("failed to load VMD from %q: %v", mpath, err),
	}
}

func newVMDMismatchErr(mainVMD, otherVMD *VMD, mpath string) *StorageIntegrityError {
	return &StorageIntegrityError{
		code: siMetaMismatch,
		msg:  fmt.Sprintf("VMD mismatch: %s vs %s (%q)", mainVMD, otherVMD, mpath),
	}
}

func siError(code int) string {
	return fmt.Sprintf(
		"storage integrity error: sie#%d - for details, see %s/blob/master/docs/troubleshooting.md",
		code, cmn.GithubHome,
	)
}
