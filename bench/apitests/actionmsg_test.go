// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package apitests

import (
	"testing"

	"github.com/NVIDIA/aistore/cmn"
	jsoniter "github.com/json-iterator/go"
)

func BenchmarkActionMsgMarshal(b *testing.B) {
	for i := 0; i < b.N; i++ {
		msg := cmn.ActionMsg{
			Name:   "test-name",
			Action: cmn.ActDelete,
			Value: &cmn.RangeMsg{
				Template: "thisisatemplate",
			},
		}
		data, err := jsoniter.Marshal(&msg)
		if err != nil {
			b.Errorf("marshaling errored: %v", err)
		}
		msg2 := &cmn.ActionMsg{}
		err = jsoniter.Unmarshal(data, &msg2)
		if err != nil {
			b.Errorf("unmarshaling errored: %v", err)
		}
		err = cmn.MorphMarshal(msg2.Value, &cmn.RangeMsg{})
		if err != nil {
			b.Errorf("morph unmarshal errored: %v", err)
		}
	}
}
