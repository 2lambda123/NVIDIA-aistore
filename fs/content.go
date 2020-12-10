// Package fs provides mountpath and FQN abstractions and methods to resolve/map stored content
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package fs

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
)

/*
 * Besides objects we must deal with additional files like: workfiles, dsort
 * intermediate files (used when spilling to disk) or EC slices. These files can
 * have different rules of rebalancing, evicting and other processing. Each
 * content type needs to implement ContentResolver to reflect the rules and
 * permission for different services. To see how the interface can be
 * implemented see: DefaultWorkfile implemention.
 *
 * When walking through the files we need to know if the file is an object or
 * other content. To do that we generate fqn with GenContentFQN. It adds short
 * prefix to the base name, which we believe is unique and will separate objects
 * from content files. We parse the file type to run ParseUniqueFQN (implemented
 * by this file type) on the rest of the base name.
 */

const (
	contentTypeLen = 2
	ObjectType     = "ob"
	WorkfileType   = "wk"
)

type (
	ContentResolver interface {
		// When set to true, services like rebalance have permission to move
		// content for example to another target because it is misplaced (HRW).
		PermToMove() bool
		// When set to true, services like LRU have permission to evict/delete content
		PermToEvict() bool
		// When set to true, content can be checksumed, shown or processed in other ways.
		PermToProcess() bool

		// Generates unique base name for original one. This function may add
		// additional information to the base name.
		// prefix - user-defined marker
		GenUniqueFQN(base, prefix string) (ufqn string)
		// Parses generated unique fqn to the original one.
		ParseUniqueFQN(base string) (orig string, old, ok bool)
	}

	PartsFQN interface {
		Bucket() cmn.Bck
		MpathInfo() *MountpathInfo
		ObjectName() string
	}

	ContentInfo struct {
		Dir  string // Original directory
		Base string // Original base name of the file
		Old  bool   // Determines if the file is old or not
		Type string // Type of the workfile
	}

	ContentSpecMgr struct {
		RegisteredContentTypes map[string]ContentResolver
	}
)

// RegisterContentType registers new content type with a given content resolver.
// NOTE: all content type registrations must happen at startup.
func (f *ContentSpecMgr) RegisterContentType(contentType string, spec ContentResolver) error {
	if strings.ContainsRune(contentType, filepath.Separator) {
		return fmt.Errorf("%s content type cannot contain %q", contentType, filepath.Separator)
	}
	if len(contentType) != contentTypeLen {
		return fmt.Errorf("%s content type must have length %d", contentType, contentTypeLen)
	}
	if _, ok := f.RegisteredContentTypes[contentType]; ok {
		return fmt.Errorf("%s content type is already registered", contentType)
	}
	f.RegisteredContentTypes[contentType] = spec
	return nil
}

// GenContentFQN returns a new FQN generated from given parts.
func (f *ContentSpecMgr) GenContentFQN(parts PartsFQN, contentType, prefix string) (fqn string) {
	var (
		spec    = f.RegisteredContentTypes[contentType]
		objName = spec.GenUniqueFQN(parts.ObjectName(), prefix)
	)
	return f.FQN(parts.MpathInfo(), parts.Bucket(), contentType, objName)
}

// FileSpec returns the specification/attributes and information about fqn. spec
// and info are only set when fqn was generated by GenContentFQN.
func (f *ContentSpecMgr) FileSpec(fqn string) (resolver ContentResolver, info *ContentInfo) {
	dir, base := filepath.Split(fqn)
	if !strings.HasSuffix(dir, "/") || base == "" {
		return
	}
	parsedFQN, err := ParseFQN(fqn)
	if err != nil {
		return
	}
	spec, found := f.RegisteredContentTypes[parsedFQN.ContentType]
	if !found {
		glog.Errorf("%q: unknown content type %s", fqn, parsedFQN.ContentType)
		return
	}
	origBase, old, ok := spec.ParseUniqueFQN(base)
	if !ok {
		return
	}
	resolver = spec
	info = &ContentInfo{Dir: dir, Base: origBase, Old: old, Type: parsedFQN.ContentType}
	return
}

func (f *ContentSpecMgr) FQN(mi *MountpathInfo, bck cmn.Bck, contentType, objName string) (fqn string) {
	return mi.MakePathFQN(bck, contentType, objName)
}

func (f *ContentSpecMgr) PermToEvict(fqn string) (ok, isOld bool) {
	spec, info := f.FileSpec(fqn)
	if spec == nil {
		return true, false
	}

	return spec.PermToEvict(), info.Old
}

func (f *ContentSpecMgr) PermToMove(fqn string) (ok bool) {
	spec, _ := f.FileSpec(fqn)
	if spec == nil {
		return false
	}

	return spec.PermToMove()
}

func (f *ContentSpecMgr) PermToProcess(fqn string) (ok bool) {
	spec, _ := f.FileSpec(fqn)
	if spec == nil {
		return false
	}

	return spec.PermToProcess()
}

// FIXME: This should be probably placed somewhere else \/

type (
	ObjectContentResolver   struct{}
	WorkfileContentResolver struct{}
)

func (wf *ObjectContentResolver) PermToMove() bool    { return true }
func (wf *ObjectContentResolver) PermToEvict() bool   { return true }
func (wf *ObjectContentResolver) PermToProcess() bool { return true }

func (wf *ObjectContentResolver) GenUniqueFQN(base, _ string) string {
	return base
}

func (wf *ObjectContentResolver) ParseUniqueFQN(base string) (orig string, old, ok bool) {
	return base, false, true
}

func (wf *WorkfileContentResolver) PermToMove() bool    { return false }
func (wf *WorkfileContentResolver) PermToEvict() bool   { return true }
func (wf *WorkfileContentResolver) PermToProcess() bool { return false }

func (wf *WorkfileContentResolver) GenUniqueFQN(base, prefix string) string {
	var (
		dir, fname = filepath.Split(base)
		tieBreaker = cmn.GenTie()
	)
	fname = prefix + "." + fname
	base = filepath.Join(dir, fname)
	return base + "." + tieBreaker + "." + spid
}

func (wf *WorkfileContentResolver) ParseUniqueFQN(base string) (orig string, old, ok bool) {
	// remove original content type
	cntIndex := strings.Index(base, ".")
	if cntIndex < 0 {
		return "", false, false
	}
	base = base[cntIndex+1:]

	pidIndex := strings.LastIndex(base, ".") // pid
	if pidIndex < 0 {
		return "", false, false
	}
	tieIndex := strings.LastIndex(base[:pidIndex], ".") // tie breaker
	if tieIndex < 0 {
		return "", false, false
	}
	filePID, err := strconv.ParseInt(base[pidIndex+1:], 16, 64)
	if err != nil {
		return "", false, false
	}

	return base[:tieIndex], filePID != pid, true
}
