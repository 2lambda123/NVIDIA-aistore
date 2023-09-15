// Package shard provides Extract(shard), Create(shard), and associated methods
// across all suppported archival formats (see cmn/archive/mime.go)
/*
 * Copyright (c) 2023, NVIDIA CORPORATION. All rights reserved.
 */
package shard

import (
	"archive/tar"
	"io"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn/archive"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/ext/dsort/ct"
	"github.com/NVIDIA/aistore/fs"
	"github.com/pierrec/lz4/v3"
)

type tlz4RW struct {
	ext string
}

// interface guard
var _ RW = (*tlz4RW)(nil)

func NewTarlz4RW() RW { return &tlz4RW{ext: archive.ExtTarLz4} }

func (*tlz4RW) IsCompressed() bool   { return true }
func (*tlz4RW) SupportsOffset() bool { return true }
func (*tlz4RW) MetadataSize() int64  { return archive.TarBlockSize } // size of tar header with padding

// Extract  the tarball f and extracts its metadata.
func (trw *tlz4RW) Extract(lom *cluster.LOM, r cos.ReadReaderAt, extractor RecordExtractor, toDisk bool) (int64, int, error) {
	ar, err := archive.NewReader(trw.ext, r)
	if err != nil {
		return 0, 0, err
	}
	workFQN := fs.CSM.Gen(lom, ct.DSortFileType, "") // tarFQN
	wfh, err := cos.CreateFile(workFQN)
	if err != nil {
		return 0, 0, err
	}

	c := &rcbCtx{parent: trw, extractor: extractor, shardName: lom.ObjName, toDisk: toDisk}
	c.tw = tar.NewWriter(wfh)
	buf, slab := T.PageMM().AllocSize(lom.SizeBytes())
	c.buf = buf

	_, err = ar.Range("", c.xtar)

	slab.Free(buf)
	if err == nil {
		cos.Close(c.tw)
	} else {
		_ = c.tw.Close()
	}
	cos.Close(wfh)

	return c.extractedSize, c.extractedCount, err
}

// Create creates a new shard locally based on the Shard.
// Note that the order of closing must be trw, lzw, then finally tarball.
func (*tlz4RW) Create(s *Shard, tarball io.Writer, loader ContentLoader) (written int64, err error) {
	var (
		n         int64
		needFlush bool
		lzw       = lz4.NewWriter(tarball)
		tw        = tar.NewWriter(lzw)
		rdReader  = newTarRecordDataReader()
	)

	defer func() {
		rdReader.free()
		cos.Close(tw)
		cos.Close(lzw)
	}()

	for _, rec := range s.Records.All() {
		for _, obj := range rec.Objects {
			switch obj.StoreType {
			case OffsetStoreType:
				if needFlush {
					// We now will write directly to the tarball file so we need
					// to flush everything what we have written so far.
					if err := tw.Flush(); err != nil {
						return written, err
					}
					needFlush = false
				}
				if n, err = loader.Load(lzw, rec, obj); err != nil {
					return written + n, err
				}
				// pad to 512 bytes
				diff := cos.CeilAlignInt64(n, archive.TarBlockSize) - n
				if diff > 0 {
					if _, err = lzw.Write(padBuf[:diff]); err != nil {
						return written + n, err
					}
					n += diff
				}
				debug.Assert(diff >= 0 && diff < archive.TarBlockSize)
			case SGLStoreType, DiskStoreType:
				rdReader.reinit(tw, obj.Size, obj.MetadataSize)
				if n, err = loader.Load(rdReader, rec, obj); err != nil {
					return written + n, err
				}
				written += n
				needFlush = true
			default:
				debug.Assert(false, obj.StoreType)
			}

			written += n
		}
	}
	return written, nil
}