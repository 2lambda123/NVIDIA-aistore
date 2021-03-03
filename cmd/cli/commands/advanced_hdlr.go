// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This file provides advanced commands that are useful for testing or development but not everyday use.
/*
 * Copyright (c) 2021, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/urfave/cli"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
	"golang.org/x/sync/errgroup"
)

var (
	advancedCmdsFlags = map[string][]cli.Flag{
		commandGenShards: {
			fileSizeFlag,
			fileCountFlag,
			cleanupFlag,
			concurrencyFlag,
		},
	}

	advancedCmds = []cli.Command{
		{
			Name:  commandAdvanced,
			Usage: "special commands intended for development and advanced usage",
			Subcommands: []cli.Command{
				{
					Name:      commandGenShards,
					Usage:     fmt.Sprintf("put randomly generated shards that can be used for %s testing", cmn.DSortName),
					ArgsUsage: `"BUCKET/TEMPLATE.EXT"`,
					Flags:     advancedCmdsFlags[commandGenShards],
					Action:    genShardsHandler,
				},
				{
					Name:   cmn.ActLoadLomCache,
					Usage:  "load object metadata into memory",
					Flags:  startCmdsFlags[subcmdStartXaction],
					Action: startXactionHandler,
				},
				{
					Name:         cmn.ActResilver,
					Usage:        "start resilvering objects across all drives on one or all targets",
					ArgsUsage:    optionalTargetIDArgument,
					Flags:        startCmdsFlags[subcmdStartXaction],
					Action:       startXactionHandler,
					BashComplete: daemonCompletions(completeTargets),
				},
				{
					Name:         subcmdPreload,
					Usage:        "preload object metadata into in-memory cache",
					ArgsUsage:    bucketArgument,
					Action:       loadLomCacheHandler,
					BashComplete: bucketCompletions(),
				},
			},
		},
	}
)

func genShardsHandler(c *cli.Context) error {
	var (
		fileCnt   = parseIntFlag(c, fileCountFlag)
		concLimit = parseIntFlag(c, concurrencyFlag)
	)

	if c.NArg() == 0 {
		return incorrectUsageMsg(c, "missing bucket and template")
	} else if c.NArg() > 1 {
		return incorrectUsageMsg(c, "too many arguments provided (make sure that provided argument is quoted to prevent bash brace expansion)")
	}

	// Expecting: "ais://bucket/shard-{00..99}.tar"
	bck, object, err := parseBckObjectURI(c, c.Args()[0])
	if err != nil {
		return err
	}
	var (
		ext      = filepath.Ext(object)
		template = strings.TrimSuffix(object, ext)
	)

	fileSize, err := parseByteFlagToInt(c, fileSizeFlag)
	if err != nil {
		return err
	}

	supportedExts := []string{".tar", ".tgz"}
	if !cmn.StringInSlice(ext, supportedExts) {
		return fmt.Errorf("extension %q is invalid, should be one of %q", ext, strings.Join(supportedExts, ", "))
	}

	mem := &memsys.MMSA{Name: "dsort-cli"}
	if err := mem.Init(false); err != nil {
		return err
	}

	pt, err := cmn.ParseBashTemplate(template)
	if err != nil {
		return err
	}

	if err := setupBucket(c, bck); err != nil {
		return err
	}

	var (
		// Progress bar
		text     = "Shards created: "
		progress = mpb.New(mpb.WithWidth(progressBarWidth))
		bar      = progress.AddBar(
			pt.Count(),
			mpb.PrependDecorators(
				decor.Name(text, decor.WC{W: len(text) + 2, C: decor.DSyncWidthR}),
				decor.CountersNoUnit("%d/%d", decor.WCSyncWidth),
			),
			mpb.AppendDecorators(decor.Percentage(decor.WCSyncWidth)),
		)

		concSemaphore = make(chan struct{}, concLimit)
		group, ctx    = errgroup.WithContext(context.Background())
		shardIt       = pt.Iter()
		shardNum      = 0
	)

CreateShards:
	for shardName, hasNext := shardIt(); hasNext; shardName, hasNext = shardIt() {
		select {
		case concSemaphore <- struct{}{}:
		case <-ctx.Done():
			break CreateShards
		}

		group.Go(func(i int, name string) func() error {
			return func() error {
				defer func() {
					bar.Increment()
					<-concSemaphore
				}()

				name := fmt.Sprintf("%s%s", name, ext)
				sgl := mem.NewSGL(fileSize * int64(fileCnt))
				defer sgl.Free()

				if err := createTar(sgl, ext, i*fileCnt, (i+1)*fileCnt, fileCnt, fileSize); err != nil {
					return err
				}

				putArgs := api.PutObjectArgs{
					BaseParams: defaultAPIParams,
					Bck:        bck,
					Object:     name,
					Reader:     sgl,
				}
				return api.PutObject(putArgs)
			}
		}(shardNum, shardName))
		shardNum++
	}

	if err := group.Wait(); err != nil {
		bar.Abort(true)
		return err
	}

	progress.Wait()
	return nil
}

func loadLomCacheHandler(c *cli.Context) (err error) {
	var bck cmn.Bck

	if c.NArg() == 0 {
		return incorrectUsageMsg(c, "missing bucket name")
	}
	if c.NArg() > 1 {
		return incorrectUsageMsg(c, "too many arguments")
	}

	if bck, err = parseBckURI(c, c.Args().First()); err != nil {
		return err
	}

	return startXaction(c, cmn.ActLoadLomCache, bck, "")
}
