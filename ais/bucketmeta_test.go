// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/jsp"
	"github.com/NVIDIA/aistore/fs"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("BMD marshal and unmarshal", func() {
	const (
		mpath    = "/tmp"
		testpath = "/tmp/.ais.test.bmd"
	)

	var (
		bmd *bucketMD
		cfg *cmn.Config
	)

	BeforeEach(func() {
		// Set path for proxy (it uses ConfigDir)
		config := cmn.GCO.BeginUpdate()
		config.ConfigDir = mpath
		config.Cksum.Type = cos.ChecksumXXHash
		cmn.GCO.CommitUpdate(config)
		cfg = cmn.GCO.Get()

		bmd = newBucketMD()
		for _, provider := range []string{cmn.ProviderAIS, cmn.ProviderAmazon} {
			for i := 0; i < 10; i++ {
				var hdr http.Header
				if provider != cmn.ProviderAIS {
					hdr = http.Header{cmn.HeaderBackendProvider: []string{provider}}
				}

				var (
					bck   = cluster.NewBck(fmt.Sprintf("bucket_%d", i), provider, cmn.NsGlobal)
					props = defaultBckProps(bckPropsArgs{bck: bck, hdr: hdr})
				)
				bmd.add(bck, props)
			}
		}
	})

	for _, node := range []string{cmn.Target, cmn.Proxy} {
		makeBMDOwner := func() bmdOwner {
			var bowner bmdOwner
			switch node {
			case cmn.Target:
				bowner = newBMDOwnerTgt()
			case cmn.Proxy:
				bowner = newBMDOwnerPrx(cfg)
			}
			return bowner
		}

		Describe(node, func() {
			var bowner bmdOwner

			BeforeEach(func() {
				bowner = makeBMDOwner()
				bowner.put(bmd)
			})

			It(fmt.Sprintf("should correctly load bmd for %s", node), func() {
				bowner.init()
				Expect(bowner.Get()).To(Equal(&bmd.BMD))
			})

			It(fmt.Sprintf("should save and load bmd using jsp methods for %s", node), func() {
				bowner.init()
				bmd := bowner.get()
				for _, signature := range []bool{false, true} {
					for _, compress := range []bool{false, true} {
						for _, checksum := range []bool{false, true} {
							opts := jsp.Options{
								Compress:  compress,
								Checksum:  checksum,
								Signature: signature,
							}
							clone := bmd.clone()
							bck := cluster.NewBck("abc"+cos.GenTie(), cmn.ProviderAIS, cmn.NsGlobal)

							// Add bucket and save.
							clone.add(bck, defaultBckProps(bckPropsArgs{bck: bck}))
							err := jsp.Save(testpath, clone, opts)
							Expect(err).NotTo(HaveOccurred())

							// Load elsewhere and check.
							loaded := newBucketMD()
							_, err = jsp.Load(testpath, loaded, opts)
							Expect(err).NotTo(HaveOccurred())
							Expect(loaded.UUID).To(BeEquivalentTo(clone.UUID))
							Expect(loaded.Version).To(BeEquivalentTo(clone.Version))
							_, present := loaded.Get(bck)
							Expect(present).To(BeTrue())
						}
					}
				}
			})

			It(fmt.Sprintf("should correctly detect bmd corruption %s", node), func() {
				bmdFullPath := filepath.Join(mpath, fs.BmdPersistedFileName)
				f, err := os.OpenFile(bmdFullPath, os.O_RDWR, 0)
				Expect(err).NotTo(HaveOccurred())
				_, err = f.WriteAt([]byte("xxxxxxxxxxxx"), 10)
				Expect(err).NotTo(HaveOccurred())
				Expect(f.Close()).NotTo(HaveOccurred())

				fmt.Println("NOTE: error on screen is expected at this point...")
				fmt.Println("")
				bowner = makeBMDOwner()
				bowner.init()

				Expect(bowner.Get()).NotTo(Equal(&bmd.BMD))
			})
		})
	}
})
