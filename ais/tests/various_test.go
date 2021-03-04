// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"net/http"
	"testing"

	"github.com/NVIDIA/aistore/devtools/tassert"
	"github.com/NVIDIA/aistore/devtools/tutils"
)

func TestInvalidHTTPMethod(t *testing.T) {
	proxyURL := tutils.RandomProxyURL(t)

	req, err := http.NewRequest("TEST", proxyURL, nil)
	tassert.CheckFatal(t, err)
	tassert.CheckResp(t, tutils.HTTPClient, req, http.StatusBadRequest)
}
