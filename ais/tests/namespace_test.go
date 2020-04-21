// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2020, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"testing"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/tutils"
	"github.com/NVIDIA/aistore/tutils/tassert"
)

func TestNamespace(t *testing.T) {
	tests := []struct {
		name   string
		remote bool
		bck1   cmn.Bck
		bck2   cmn.Bck
	}{
		{
			name:   "global_and_local_namespace",
			remote: false,
			bck1: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
			},
			bck2: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
				Ns: cmn.Ns{
					UUID: "",
					Name: "namespace",
				},
			},
		},
		{
			name:   "two_local_namespaces",
			remote: false,
			bck1: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
				Ns: cmn.Ns{
					UUID: "",
					Name: "ns1",
				},
			},
			bck2: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
				Ns: cmn.Ns{
					UUID: "",
					Name: "ns2",
				},
			},
		},
		{
			name:   "global_namespaces_with_remote_cluster",
			remote: true,
			bck1: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
			},
			bck2: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
				Ns: cmn.Ns{
					UUID: tutils.RemoteCluster.UUID,
					Name: cmn.NsGlobal.Name,
				},
			},
		},
		{
			name:   "namespaces_with_remote_cluster",
			remote: true,
			bck1: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
				Ns: cmn.Ns{
					UUID: "",
					Name: "ns1",
				},
			},
			bck2: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
				Ns: cmn.Ns{
					UUID: tutils.RemoteCluster.UUID,
					Name: "ns1",
				},
			},
		},
		{
			name:   "namespaces_with_only_remote_cluster",
			remote: true,
			bck1: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
				Ns: cmn.Ns{
					UUID: tutils.RemoteCluster.UUID,
					Name: "ns1",
				},
			},
			bck2: cmn.Bck{
				Name:     "tmp",
				Provider: cmn.ProviderAIS,
				Ns: cmn.Ns{
					UUID: tutils.RemoteCluster.UUID,
					Name: "ns2",
				},
			},
		},
	}

	var (
		proxyURL   = tutils.GetPrimaryURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
	)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				m1 = ioContext{
					t:   t,
					num: 100,
					bck: test.bck1,
				}
				m2 = ioContext{
					t:   t,
					num: 200,
					bck: test.bck2,
				}
			)

			tutils.CheckSkip(t, tutils.SkipTestArgs{
				RequiresRemote: test.remote,
			})

			m1.init()
			m2.init()

			err := api.CreateBucket(baseParams, m1.bck)
			tassert.CheckFatal(t, err)
			err = api.CreateBucket(baseParams, m2.bck)
			tassert.CheckFatal(t, err)

			defer func() {
				err := api.DestroyBucket(baseParams, m2.bck)
				tassert.CheckFatal(t, err)
				err = api.DestroyBucket(baseParams, m1.bck)
				tassert.CheckFatal(t, err)
			}()

			// Test listing buckets
			buckets, err := api.ListBuckets(baseParams, cmn.QueryBcks{Provider: cmn.ProviderAIS})
			tassert.CheckFatal(t, err)
			if test.remote {
				remoteBuckets, err := api.ListBuckets(baseParams, cmn.QueryBcks{
					Provider: cmn.ProviderAIS,
					Ns:       cmn.NsAnyRemote,
				})
				tassert.CheckFatal(t, err)
				buckets = append(buckets, remoteBuckets...)

				// Make sure that listing with specific UUID also works and have
				// similar outcome.
				remoteClusterBuckets, err := api.ListBuckets(baseParams, cmn.QueryBcks{
					Provider: cmn.ProviderAIS,
					Ns:       cmn.Ns{UUID: tutils.RemoteCluster.UUID},
				})
				tassert.CheckFatal(t, err)
				// NOTE: we cannot do `remoteClusterBuckets.Equal(remoteBuckets)` because
				//  they will most probably have different `Ns.UUID` (alias vs uuid).
				tassert.Fatalf(
					t, len(remoteClusterBuckets) == len(remoteBuckets),
					"remote buckets do not match expected: %v, got: %v", remoteClusterBuckets, remoteBuckets,
				)
			}
			tassert.Fatalf(
				t, len(buckets) == 2,
				"number of buckets (%d) should be equal to 2", len(buckets),
			)

			m1.puts()
			m2.puts()

			// Now remote bucket(s) should be present in BMD
			buckets, err = api.ListBuckets(baseParams, cmn.QueryBcks{Provider: cmn.ProviderAIS})
			tassert.CheckFatal(t, err)
			tassert.Fatalf(
				t, len(buckets) == 2,
				"number of buckets (%d) should be equal to 2", len(buckets),
			)

			// Test listing objects
			objects, err := api.ListObjects(baseParams, m1.bck, nil, 0)
			tassert.CheckFatal(t, err)
			tassert.Errorf(
				t, len(objects.Entries) == m1.num,
				"number of entries (%d) should be equal to (%d)", len(objects.Entries), m1.num,
			)

			objects, err = api.ListObjects(baseParams, m2.bck, nil, 0)
			tassert.CheckFatal(t, err)
			tassert.Errorf(
				t, len(objects.Entries) == m2.num,
				"number of entries (%d) should be equal to (%d)", len(objects.Entries), m2.num,
			)

			// Test summary
			summaries, err := api.GetBucketsSummaries(baseParams, cmn.QueryBcks{Provider: cmn.ProviderAIS}, nil)
			tassert.CheckFatal(t, err)
			tassert.Errorf(
				t, len(summaries) == 2,
				"number of summaries (%d) should be equal to 2", len(summaries),
			)

			for _, summary := range summaries {
				if summary.Bck.Equal(m1.bck) {
					tassert.Errorf(
						t, summary.ObjCount == uint64(m1.num),
						"number of objects (%d) should be equal to (%d)", summary.ObjCount, m1.num,
					)
				} else if summary.Bck.Equal(m2.bck) {
					tassert.Errorf(
						t, summary.ObjCount == uint64(m2.num),
						"number of objects (%d) should be equal to (%d)", summary.ObjCount, m2.num,
					)
				} else {
					t.Errorf("unknown bucket in summary: %q", summary.Bck)
				}
			}

			m1.gets()
			m2.gets()

			m1.ensureNoErrors()
			m2.ensureNoErrors()
		})
	}
}

func TestRemoteWithAliasAndUUID(t *testing.T) {
	var (
		smap = tutils.GetClusterMap(t, tutils.RemoteCluster.URL)

		alias = tutils.RemoteCluster.UUID
		uuid  = smap.UUID
	)

	tutils.CheckSkip(t, tutils.SkipTestArgs{
		RequiresRemote: true,
	})
	if alias == uuid {
		t.Skipf("expected %q to be alias of cluster uuid: %q", alias, uuid)
	}

	// TODO: make it work
	t.Skip("NYI")

	var (
		proxyURL   = tutils.GetPrimaryURL()
		baseParams = tutils.BaseAPIParams(proxyURL)

		m1 = ioContext{
			t:   t,
			num: 100,
			bck: cmn.Bck{Name: "tmp", Ns: cmn.Ns{UUID: alias}},
		}
		m2 = ioContext{
			t:   t,
			num: 200,
			bck: cmn.Bck{Name: "tmp", Ns: cmn.Ns{UUID: uuid}},
		}
	)

	m1.init()
	m2.init()

	err := api.CreateBucket(baseParams, m1.bck)
	tassert.CheckFatal(t, err)
	defer func() {
		err := api.DestroyBucket(baseParams, m1.bck)
		tassert.CheckFatal(t, err)
	}()

	m1.puts()
	m2.puts()

	// TODO: works until this point

	buckets, err := api.ListBuckets(baseParams, cmn.QueryBcks{Provider: cmn.ProviderAIS})
	tassert.CheckFatal(t, err)
	tassert.Fatalf(
		t, len(buckets) == 1,
		"number of buckets (%d) should be equal to 1", len(buckets),
	)

	for _, bck := range []cmn.Bck{m1.bck, m2.bck} {
		objects, err := api.ListObjects(baseParams, bck, nil, 0)
		tassert.CheckFatal(t, err)
		tassert.Errorf(
			t, len(objects.Entries) == m1.num+m2.num,
			"number of entries (%d) should be equal to (%d)", len(objects.Entries), m1.num+m2.num,
		)
	}
}

func TestRemoteWithSilentBucketDestroy(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{
		RequiresRemote: true,
	})

	// TODO: make it work
	t.Skip("NYI")

	var (
		proxyURL   = tutils.GetPrimaryURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		remoteBP   = tutils.BaseAPIParams(tutils.RemoteCluster.URL)

		m = ioContext{
			t:   t,
			num: 100,
			bck: cmn.Bck{Ns: cmn.Ns{UUID: tutils.RemoteCluster.UUID}},
		}
	)

	m.init()

	err := api.CreateBucket(baseParams, m.bck)
	tassert.CheckFatal(t, err)
	defer func() {
		// Delete just in case something goes wrong (therefore ignoring error)
		api.DestroyBucket(baseParams, m.bck)
	}()

	m.puts()
	m.gets()

	err = api.DestroyBucket(remoteBP, cmn.Bck{Name: m.bck.Name})
	tassert.CheckFatal(t, err)

	// Check that bucket is still cached
	buckets, err := api.ListBuckets(baseParams, cmn.QueryBcks{Provider: cmn.ProviderAIS})
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(buckets) == 1, "number of buckets (%d) should be equal to 1", len(buckets))

	// Test listing objects
	_, err = api.ListObjects(baseParams, m.bck, nil, 0)
	tassert.Fatalf(t, err != nil, "expected listing objects to error (bucket does not exist)")

	// TODO: it works until this point

	// Check that bucket is no longer present
	buckets, err = api.ListBuckets(baseParams, cmn.QueryBcks{Provider: cmn.ProviderAIS})
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(buckets) == 0, "number of buckets (%d) should be equal to 0", len(buckets))
}
