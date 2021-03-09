// Package cmn provides common constants, types, and utilities for AIS clients
// and AIStore.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
)

// Backend Provider enum
const (
	ProviderAIS    = "ais"
	ProviderAmazon = "aws"
	ProviderAzure  = "azure"
	ProviderGoogle = "gcp"
	ProviderHDFS   = "hdfs"
	ProviderHTTP   = "ht"
	allProviders   = "ais, aws (s3://), gcp (gs://), azure (az://), hdfs://, ht://"

	NsUUIDPrefix = '@' // BEWARE: used by on-disk layout
	NsNamePrefix = '#' // BEWARE: used by on-disk layout

	BckProviderSeparator = "://"

	// Scheme parsing
	DefaultScheme = "https"
	GSScheme      = "gs"
	S3Scheme      = "s3"
	AZScheme      = "az"
	AISScheme     = "ais"
)

type (
	// Ns (or Namespace) adds additional layer for scoping the data under
	// the same provider. It allows to have same dataset and bucket names
	// under different namespaces what allows for easy data manipulation without
	// affecting data in different namespaces.
	Ns struct {
		// UUID of other remote AIS cluster (for now only used for AIS). Note
		// that we can have different namespaces which refer to same UUID (cluster).
		// This means that in a sense UUID is a parent of the actual namespace.
		UUID string `json:"uuid" yaml:"uuid"`
		// Name uniquely identifies a namespace under the same UUID (which may
		// be empty) and is used in building FQN for the objects.
		Name string `json:"name" yaml:"name"`
	}

	Bck struct {
		Name     string       `json:"name" yaml:"name"`
		Provider string       `json:"provider" yaml:"provider"`
		Ns       Ns           `json:"namespace" yaml:"namespace" list:"omitempty"`
		Props    *BucketProps `json:"-"`
	}

	// Represents the AIS bucket, object and URL associated with a HTTP resource
	HTTPBckObj struct {
		Bck        Bck
		ObjName    string
		OrigURLBck string // HTTP URL of the bucket (object name excluded)
	}

	QueryBcks Bck

	BucketNames []Bck

	// implemented by cluster.Bck
	NLP interface {
		Lock()
		TryLock(timeout time.Duration) bool
		TryRLock(timeout time.Duration) bool
		Unlock()
	}
)

var (
	// NsGlobal represents *this* cluster's global namespace that is used by default when
	// no specific namespace was defined or provided by the user.
	NsGlobal = Ns{}
	// NsAnyRemote represents any remote cluster. As such, NsGlobalRemote applies
	// exclusively to AIS (provider) given that other Backend providers are remote by definition.
	NsAnyRemote = Ns{UUID: string(NsUUIDPrefix)}

	Providers = cos.NewStringSet(
		ProviderAIS,
		ProviderGoogle,
		ProviderAmazon,
		ProviderAzure,
		ProviderHDFS,
		ProviderHTTP,
	)
)

// Parses [@uuid][#namespace]. It does a little bit more than just parsing
// a string from `Uname` so that logic can be reused in different places.
func ParseNsUname(s string) (n Ns) {
	if len(s) > 0 && s[0] == NsUUIDPrefix {
		s = s[1:]
	}
	idx := strings.IndexByte(s, NsNamePrefix)
	if idx == -1 {
		n.UUID = s
	} else {
		n.UUID = s[:idx]
		n.Name = s[idx+1:]
	}
	return
}

// IsNormalizedProvider returns true if the provider is in normalized
// form (`aws`, `gcp`, etc.), not aliased (`s3`, `gs`, etc.). Only providers
// registered in `Providers` set are considered normalized.
func IsNormalizedProvider(provider string) bool {
	_, exists := Providers[provider]
	return exists
}

// NormalizeProvider replaces provider aliases with their normalized form/name.
func NormalizeProvider(provider string) (string, error) {
	switch provider {
	case "":
		return "", nil
	case S3Scheme:
		return ProviderAmazon, nil
	case AZScheme:
		return ProviderAzure, nil
	case GSScheme:
		return ProviderGoogle, nil
	default:
		if !IsNormalizedProvider(provider) {
			return provider, NewErrorInvalidBucketProvider(Bck{Provider: provider})
		}
		return provider, nil
	}
}

// Parses "[provider://][@uuid#namespace][/][bucketName[/objectName]]"
func ParseBckObjectURI(objName string, query ...bool) (bck Bck, object string, err error) {
	const bucketSepa = "/"
	parts := strings.SplitN(objName, BckProviderSeparator, 2)

	if len(parts) > 1 {
		bck.Provider, err = NormalizeProvider(parts[0])
		objName = parts[1]
	}

	if err != nil {
		return
	}

	parts = strings.SplitN(objName, bucketSepa, 2)
	if len(parts[0]) > 0 && (parts[0][0] == NsUUIDPrefix || parts[0][0] == NsNamePrefix) {
		bck.Ns = ParseNsUname(parts[0])
		if err := bck.Ns.Validate(); err != nil {
			return bck, "", err
		}
		if len(parts) == 1 {
			if bck.Provider == "" {
				bck.Provider = ProviderAIS // Always default to `ais://` provider.
			}
			isQuery := len(query) > 0 && query[0]
			if parts[0] == string(NsUUIDPrefix) && isQuery {
				// Case: "[provider://]@" (only valid if uri is query)
				// We need to list buckets from all possible remote clusters
				bck.Ns = NsAnyRemote
				return bck, "", nil
			}

			// Case: "[provider://]@uuid#ns"
			return bck, "", nil
		}

		// Case: "[provider://]@uuid#ns/bucket"
		parts = strings.SplitN(parts[1], bucketSepa, 2)
	}

	bck.Name = parts[0]
	if bck.Name != "" {
		if err := bck.ValidateName(); err != nil {
			return bck, "", err
		}
		if bck.Provider == "" {
			bck.Provider = ProviderAIS // Always default to `ais://` provider.
		}
	}
	if len(parts) > 1 {
		object = parts[1]
	}
	return
}

////////
// Ns //
////////

func (n Ns) String() string {
	if n.IsGlobal() {
		return ""
	}
	res := ""
	if n.UUID != "" {
		res += string(NsUUIDPrefix) + n.UUID
	}
	if n.Name != "" {
		res += string(NsNamePrefix) + n.Name
	}
	return res
}

func (n Ns) Uname() string {
	b := make([]byte, 0, 2+len(n.UUID)+len(n.Name))
	b = append(b, NsUUIDPrefix)
	b = append(b, n.UUID...)
	b = append(b, NsNamePrefix)
	b = append(b, n.Name...)
	return string(b)
}

func (n Ns) Validate() error {
	if !nsReg.MatchString(n.UUID) || !nsReg.MatchString(n.Name) {
		return fmt.Errorf(
			"namespace (uuid: %q, name: %q) may only contain letters, numbers, dashes (-), underscores (_)",
			n.UUID, n.Name,
		)
	}
	return nil
}

func (n Ns) Contains(other Ns) bool {
	if n.IsGlobal() {
		return true // If query is empty (global) we accept any namespace
	}
	if n.IsAnyRemote() {
		return other.IsRemote()
	}
	return n == other
}

/////////
// Bck //
/////////

func (b Bck) Less(other Bck) bool {
	if QueryBcks(b).Contains(other) {
		return true
	}
	if b.Provider != other.Provider {
		return b.Provider < other.Provider
	}
	sb, so := b.Ns.String(), other.Ns.String()
	if sb != so {
		return sb < so
	}
	return b.Name < other.Name
}

func (b Bck) Equal(other Bck) bool {
	return b.Name == other.Name && b.Provider == other.Provider && b.Ns == other.Ns
}

func (b *Bck) Valid() bool {
	return b.Validate() == nil
}

func (b *Bck) Validate() (err error) {
	if err := b.ValidateName(); err != nil {
		return err
	}
	b.Provider, err = NormalizeProvider(b.Provider)
	if err != nil {
		return err
	}
	return b.Ns.Validate()
}

func (b *Bck) ValidateName() (err error) {
	const nameHelp = "may only contain letters, numbers, dashes (-), underscores (_), and dots (.)"
	if b.Name == "" || b.Name == "." || !bucketReg.MatchString(b.Name) {
		return fmt.Errorf("bucket name %q is invalid: %v", b.Name, nameHelp)
	}
	if strings.Contains(b.Name, "..") {
		return fmt.Errorf("bucket name %q cannot contain '..'", b.Name)
	}
	return
}

func (b Bck) String() string {
	if b.Ns.IsGlobal() {
		if b.Provider == "" {
			return b.Name
		}
		return fmt.Sprintf("%s%s%s", b.Provider, BckProviderSeparator, b.Name)
	}
	if b.Provider == "" {
		return fmt.Sprintf("%s/%s", b.Ns, b.Name)
	}
	return fmt.Sprintf("%s%s%s/%s", b.Provider, BckProviderSeparator, b.Ns, b.Name)
}

func (b Bck) IsEmpty() bool { return b.Name == "" && b.Provider == "" && b.Ns == NsGlobal }

// Bck => unique name (use ParseUname below to translate back)
func (b Bck) MakeUname(objName string) string {
	var (
		nsUname = b.Ns.Uname()
		l       = len(b.Provider) + 1 + len(nsUname) + 1 + len(b.Name) + 1 + len(objName)
		buf     = make([]byte, 0, l)
	)
	buf = append(buf, b.Provider...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, nsUname...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, b.Name...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, objName...)
	return *(*string)(unsafe.Pointer(&buf))
}

// unique name => Bck (use MakeUname above to perform the reverse translation)
func ParseUname(uname string) (b Bck, objName string) {
	var prev, itemIdx int
	for i := 0; i < len(uname); i++ {
		if uname[i] != filepath.Separator {
			continue
		}

		item := uname[prev:i]
		switch itemIdx {
		case 0:
			b.Provider = item
		case 1:
			b.Ns = ParseNsUname(item)
		case 2:
			b.Name = item
			objName = uname[i+1:]
			return
		}

		itemIdx++
		prev = i + 1
	}
	return
}

//
// Is-Whats
//

func (n Ns) IsGlobal() bool    { return n == NsGlobal }
func (n Ns) IsAnyRemote() bool { return n == NsAnyRemote }
func (n Ns) IsRemote() bool    { return n.UUID != "" }

func (b *Bck) HasBackendBck() bool {
	return b.Provider == ProviderAIS && b.Props != nil && !b.Props.BackendBck.IsEmpty()
}

func (b *Bck) BackendBck() *Bck {
	if b.HasBackendBck() {
		return &b.Props.BackendBck
	}
	return nil
}

func (b *Bck) RemoteBck() *Bck {
	if !b.IsRemote() {
		return nil
	}
	if b.HasBackendBck() {
		return &b.Props.BackendBck
	}
	return b
}

func (b Bck) IsAIS() bool       { return b.Provider == ProviderAIS && !b.Ns.IsRemote() && !b.HasBackendBck() }
func (b Bck) IsRemoteAIS() bool { return b.Provider == ProviderAIS && b.Ns.IsRemote() }
func (b Bck) IsHDFS() bool      { return b.Provider == ProviderHDFS }
func (b Bck) IsHTTP() bool      { return b.Provider == ProviderHTTP }

func (b Bck) IsRemote() bool {
	return b.IsCloud() || b.IsRemoteAIS() || b.IsHDFS() || b.IsHTTP() || b.HasBackendBck()
}

func (b Bck) IsCloud() bool {
	if bck := b.BackendBck(); bck != nil {
		debug.Assert(bck.IsCloud()) // Currently, backend bucket is always cloud.
		return bck.IsCloud()
	}
	return b.Provider == ProviderAmazon || b.Provider == ProviderAzure || b.Provider == ProviderGoogle
}

func (b Bck) HasProvider() bool {
	if b.Provider != "" {
		// If the provider is set it must be valid.
		debug.Assert(IsNormalizedProvider(b.Provider))
		return true
	}
	return false
}

func (query QueryBcks) String() string    { return Bck(query).String() }
func (query QueryBcks) IsAIS() bool       { return Bck(query).IsAIS() }
func (query QueryBcks) IsHDFS() bool      { return Bck(query).IsHDFS() }
func (query QueryBcks) IsRemoteAIS() bool { return Bck(query).IsRemoteAIS() }
func (query *QueryBcks) Validate() (err error) {
	if query.Name != "" {
		bck := Bck(*query)
		if err := bck.ValidateName(); err != nil {
			return err
		}
	}
	if query.Provider != "" {
		query.Provider, err = NormalizeProvider(query.Provider)
		if err != nil {
			return err
		}
	}
	if query.Ns != NsGlobal && query.Ns != NsAnyRemote {
		return query.Ns.Validate()
	}
	return nil
}
func (query QueryBcks) Equal(bck Bck) bool { return Bck(query).Equal(bck) }
func (query QueryBcks) Contains(other Bck) bool {
	if query.Name != "" {
		// NOTE: named bucket with no provider is assumed to be ais://
		if other.Provider == "" {
			other.Provider = ProviderAIS
		}
		if query.Provider == "" {
			// If query's provider not set, we should match the expected bucket
			query.Provider = other.Provider
		}
		return query.Equal(other)
	}
	ok := query.Provider == other.Provider || query.Provider == ""
	return ok && query.Ns.Contains(other.Ns)
}

func AddBckToQuery(query url.Values, bck Bck) url.Values {
	if bck.Provider != "" {
		if query == nil {
			query = make(url.Values)
		}
		query.Set(URLParamProvider, bck.Provider)
	}
	if !bck.Ns.IsGlobal() {
		if query == nil {
			query = make(url.Values)
		}
		query.Set(URLParamNamespace, bck.Ns.Uname())
	}
	return query
}

func AddBckUnameToQuery(query url.Values, bck Bck, uparam string) url.Values {
	if query == nil {
		query = make(url.Values)
	}
	uname := bck.MakeUname("")
	query.Set(uparam, uname)
	return query
}

func DelBckFromQuery(query url.Values) url.Values {
	query.Del(URLParamProvider)
	query.Del(URLParamNamespace)
	return query
}

/////////////////
// BucketNames //
/////////////////

func (names BucketNames) Len() int {
	return len(names)
}

func (names BucketNames) Less(i, j int) bool {
	return names[i].Less(names[j])
}

func (names BucketNames) Swap(i, j int) {
	names[i], names[j] = names[j], names[i]
}

func (names BucketNames) Select(query QueryBcks) (filtered BucketNames) {
	for _, bck := range names {
		if query.Contains(bck) {
			filtered = append(filtered, bck)
		}
	}
	return filtered
}

func (names BucketNames) Contains(query QueryBcks) bool {
	for _, bck := range names {
		if query.Equal(bck) || query.Contains(bck) {
			return true
		}
	}
	return false
}

func (names BucketNames) Equal(other BucketNames) bool {
	if len(names) != len(other) {
		return false
	}
	for _, b1 := range names {
		var found bool
		for _, b2 := range other {
			if b1.Equal(b2) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

////////////////
// HTTPBckObj //
////////////////

func NewHTTPObj(u *url.URL) *HTTPBckObj {
	hbo := &HTTPBckObj{
		Bck: Bck{
			Provider: ProviderHTTP,
			Ns:       NsGlobal,
		},
	}
	hbo.OrigURLBck, hbo.ObjName = filepath.Split(u.Path)
	hbo.OrigURLBck = u.Scheme + "://" + u.Host + hbo.OrigURLBck
	hbo.Bck.Name = cos.OrigURLBck2Name(hbo.OrigURLBck)
	return hbo
}

func NewHTTPObjPath(rawURL string) (*HTTPBckObj, error) {
	urlObj, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, err
	}
	return NewHTTPObj(urlObj), nil
}
