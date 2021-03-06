// Package test provides tests for common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package tests

import (
	"fmt"
	"testing"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/devtools/tassert"
)

type discardEntriesTestCase struct {
	entries []*cmn.BucketEntry
	size    int
}

func generateEntries(size int) []*cmn.BucketEntry {
	result := make([]*cmn.BucketEntry, 0, size)
	for i := 0; i < size; i++ {
		result = append(result, &cmn.BucketEntry{Name: fmt.Sprintf("%d", i)})
	}
	return result
}

func TestDiscardFirstEntries(t *testing.T) {
	testCases := []discardEntriesTestCase{
		{generateEntries(100), 1},
		{generateEntries(1), 1},
		{generateEntries(100), 0},
		{generateEntries(1), 0},
		{generateEntries(100), 50},
		{generateEntries(1), 50},
		{generateEntries(100), 100},
		{generateEntries(100), 150},
	}

	for _, tc := range testCases {
		t.Logf("testcase %d/%d", len(tc.entries), tc.size)
		original := append([]*cmn.BucketEntry(nil), tc.entries...)
		entries := cmn.DiscardFirstEntries(tc.entries, tc.size)
		expSize := cos.Max(0, len(original)-tc.size)
		tassert.Errorf(t, len(entries) == expSize, "incorrect size. expected %d; got %d", expSize, len(entries))
		if len(entries) > 0 {
			tassert.Errorf(t, entries[0] == original[tc.size], "incorrect elements. expected %s, got %s", entries[0].Name, original[tc.size].Name)
		}
	}
}
