// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Daniel Theophanes (kardianos@gmail.com)

package server

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cloudfoundry/gosigar"

	"github.com/cockroachdb/cockroach/base"
	"github.com/cockroachdb/cockroach/gossip/resolver"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/security"
	"github.com/cockroachdb/cockroach/storage"
	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/cockroachdb/cockroach/util/stop"
)

// Context defaults.
const (
	defaultCGroupMemPath      = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
	defaultAddr               = ":" + base.CockroachPort
	defaultPGAddr             = ":" + base.PGPort
	defaultMaxOffset          = 250 * time.Millisecond
	defaultCacheSize          = 512 << 20 // 512 MB
	defaultMemtableBudget     = 512 << 20 // 512 MB
	defaultScanInterval       = 10 * time.Minute
	defaultScanMaxIdleTime    = 5 * time.Second
	defaultMetricsFrequency   = 10 * time.Second
	defaultTimeUntilStoreDead = 5 * time.Minute
	defaultBalanceMode        = storage.BalanceModeUsage
)

// Context holds parameters needed to setup a server.
// Calling "cli".initFlags(ctx *Context) will initialize Context using
// command flags. Keep in sync with "cli/flags.go".
type Context struct {
	// Embed the base context.
	base.Context

	// Addr is the host:port to bind for HTTP/RPC traffic.
	Addr string

	// PGAddr is the host:port to bind for Postgres traffic.
	PGAddr string

	// Stores is specified to enable durable key-value storage.
	// Memory-backed key value stores may be optionally specified
	// via mem=<integer byte size>.
	//
	// Stores specify a comma-separated list of stores specified by a
	// colon-separated list of device attributes followed by '=' and
	// either a filepath for a persistent store or an integer size in bytes for an
	// in-memory store. Device attributes typically include whether the store is
	// flash (ssd), spinny disk (hdd), fusion-io (fio), in-memory (mem); device
	// attributes might also include speeds and other specs (7200rpm, 200kiops, etc.).
	// For example, -store=hdd:7200rpm=/mnt/hda1,ssd=/mnt/ssd01,ssd=/mnt/ssd02,mem=1073741824
	Stores string

	// Attrs specifies a colon-separated list of node topography or machine
	// capabilities, used to match capabilities or location preferences specified
	// in zone configs.
	Attrs string

	// Maximum clock offset for the cluster.
	MaxOffset time.Duration

	// JoinUsing is a comma-separated list of node addresses that
	// act as bootstrap hosts for connecting to the gossip network.
	JoinUsing string

	// Enables linearizable behaviour of operations on this node by making sure
	// that no commit timestamp is reported back to the client until all other
	// node clocks have necessarily passed it.
	Linearizable bool

	// CacheSize is the amount of memory in bytes to use for caching data.
	// The value is split evenly between the stores if there are more than one.
	CacheSize uint64

	// MemtableBudget is the amount of memory in bytes to use for the memory
	// table. The value is split evenly between the stores if there are more than one.
	MemtableBudget uint64

	// BalanceMode determines how this node makes balancing decisions.
	BalanceMode storage.BalanceMode

	// Parsed values.

	// Engines is the storage instances specified by Stores.
	Engines []engine.Engine

	// NodeAttributes is the parsed representation of Attrs.
	NodeAttributes roachpb.Attributes

	// GossipBootstrapResolvers is a list of gossip resolvers used
	// to find bootstrap nodes for connecting to the gossip network.
	GossipBootstrapResolvers []resolver.Resolver

	// ScanInterval determines a duration during which each range should be
	// visited approximately once by the range scanner.
	ScanInterval time.Duration

	// ScanMaxIdleTime is the maximum time the scanner will be idle between ranges.
	// If enabled (> 0), the scanner may complete in less than ScanInterval for small
	// stores.
	ScanMaxIdleTime time.Duration

	// MetricsFrequency determines the frequency at which the server should
	// record internal metrics.
	MetricsFrequency time.Duration

	// TimeUntilStoreDead is the time after which if there is no new gossiped
	// information about a store, it is considered dead.
	TimeUntilStoreDead time.Duration
}

func getDefaultCacheSize() uint64 {
	mem := sigar.Mem{}
	if err := mem.Get(); err != nil {
		if log.V(1) {
			log.Infof("can't retrieve system memory information (%s), setting default rocksdb cache size to %dMB", err, defaultCacheSize>>20)
		}
		return defaultCacheSize
	}

	halfSysMem := mem.Total / 2
	if runtime.GOOS == "linux" {
		buf, err := ioutil.ReadFile(defaultCGroupMemPath)
		if err != nil {
			if log.V(1) {
				log.Infof("can't read available memory from cgroups (%s), setting default rocksdb cache size to %dMB (half of system memory)", err, halfSysMem>>20)
			}
			return halfSysMem
		}

		cgAvlMem, err := strconv.ParseUint(strings.TrimSpace(string(buf)), 10, 64)
		if err != nil {
			if log.V(1) {
				log.Infof("can't parse available memory from cgroups (%s), setting default rocksdb cache size to %dMB (half of system memory)", err, halfSysMem>>20)
			}
			return halfSysMem
		}
		if cgAvlMem < mem.Total {
			return cgAvlMem / 2
		}
	}
	return halfSysMem
}

// NewContext returns a Context with default values.
func NewContext() *Context {
	ctx := &Context{}
	ctx.InitDefaults()
	return ctx
}

// InitDefaults sets up the default values for a Context.
func (ctx *Context) InitDefaults() {
	ctx.Context.InitDefaults()
	ctx.Addr = defaultAddr
	ctx.PGAddr = defaultPGAddr
	ctx.MaxOffset = defaultMaxOffset
	ctx.CacheSize = getDefaultCacheSize()
	ctx.MemtableBudget = defaultMemtableBudget
	ctx.ScanInterval = defaultScanInterval
	ctx.ScanMaxIdleTime = defaultScanMaxIdleTime
	ctx.MetricsFrequency = defaultMetricsFrequency
	ctx.TimeUntilStoreDead = defaultTimeUntilStoreDead
	ctx.BalanceMode = defaultBalanceMode
}

var storesRE = regexp.MustCompile(`(?:([^,=]+)=)?([^=,]+)(,|$)`)

// InitStores interprets the stores parameter to initialize a slice of
// engine.Engine objects.
func (ctx *Context) InitStores(stopper *stop.Stopper) error {
	ctx.Stores = strings.TrimSpace(ctx.Stores)
	if len(ctx.Stores) == 0 {
		return fmt.Errorf("no storage specified; see --stores")
	}
	storeSpecs := storesRE.FindAllStringSubmatch(ctx.Stores, -1)
	// Error if regexp doesn't match.
	if storeSpecs == nil {
		return fmt.Errorf("invalid storage specification %q; see --stores", ctx.Stores)
	}

	for _, storeSpec := range storeSpecs {
		name := storeSpec[0]
		if len(storeSpec) != 4 {
			return util.Errorf("unable to parse attributes and path from store %q", name)
		}
		attrs, path := storeSpec[1], storeSpec[2]
		// There are two matches for each store specification: the colon-separated
		// list of attributes and the path.
		engine, err := ctx.initEngine(attrs, path, stopper)
		if err != nil {
			return util.Errorf("unable to init engine for store %q: %s", name, err)
		}
		ctx.Engines = append(ctx.Engines, engine)
	}
	log.Infof("%d storage engine(s) specified", len(ctx.Engines))
	return nil
}

// InitNode parses node attributes and initializes the gossip bootstrap
// resolvers.
func (ctx *Context) InitNode() error {
	// Initialize attributes.
	ctx.NodeAttributes = parseAttributes(ctx.Attrs)

	// Get the gossip bootstrap resolvers.
	resolvers, err := ctx.parseGossipBootstrapResolvers()
	if err != nil {
		return err
	}
	if len(resolvers) > 0 {
		ctx.GossipBootstrapResolvers = resolvers
	}

	return nil
}

// AdminURL returns the URL for the admin UI.
func (ctx *Context) AdminURL() string {
	return fmt.Sprintf("%s://%s", ctx.HTTPRequestScheme(), ctx.Addr)
}

// PGURL returns the URL for the postgres endpoint.
func (ctx *Context) PGURL(user string) string {
	// Try to convert path to an absolute path. Failing to do so return path
	// unchanged.
	absPath := func(path string) string {
		r, err := filepath.Abs(path)
		if err != nil {
			return path
		}
		return r
	}

	options := url.Values{}
	if ctx.Insecure {
		options.Add("sslmode", "disable")
	} else {
		options.Add("sslmode", "verify-full")
		options.Add("sslcert", absPath(security.ClientCertPath(ctx.Certs, user)))
		options.Add("sslkey", absPath(security.ClientKeyPath(ctx.Certs, user)))
		options.Add("sslrootcert", absPath(security.CACertPath(ctx.Certs)))
	}
	pgURL := url.URL{
		Scheme:   "postgresql",
		User:     url.User(user),
		Host:     ctx.PGAddr,
		RawQuery: options.Encode(),
	}
	return pgURL.String()
}

var errUnsizedInMemStore = errors.New("unable to initialize an in-memory store with capacity 0")

// initEngine parses the store attributes as a colon-separated list
// and instantiates an engine based on the dir parameter. If dir parses
// to an integer, it's taken to mean an in-memory engine; otherwise,
// dir is treated as a path and a RocksDB engine is created.
func (ctx *Context) initEngine(attrsStr, path string, stopper *stop.Stopper) (engine.Engine, error) {
	attrs := parseAttributes(attrsStr)
	if size, err := strconv.ParseUint(path, 10, 64); err == nil {
		if size == 0 {
			return nil, errUnsizedInMemStore
		}
		return engine.NewInMem(attrs, size, stopper), nil
	}
	// TODO(peter): The comments and docs say that CacheSize and MemtableBudget
	// are split evenly if there are multiple stores, but we aren't doing that
	// currently.
	return engine.NewRocksDB(attrs, path, ctx.CacheSize, ctx.MemtableBudget, stopper), nil
}

// parseGossipBootstrapResolvers parses a comma-separated list of
// gossip bootstrap resolvers.
func (ctx *Context) parseGossipBootstrapResolvers() ([]resolver.Resolver, error) {
	var bootstrapResolvers []resolver.Resolver
	addresses := strings.Split(ctx.JoinUsing, ",")
	for _, address := range addresses {
		if len(address) == 0 {
			continue
		}
		resolver, err := resolver.NewResolver(&ctx.Context, address)
		if err != nil {
			return nil, err
		}
		bootstrapResolvers = append(bootstrapResolvers, resolver)
	}

	return bootstrapResolvers, nil
}

// parseAttributes parses a colon-separated list of strings,
// filtering empty strings (i.e. "::" will yield no attributes.
// Returns the list of strings as Attributes.
func parseAttributes(attrsStr string) roachpb.Attributes {
	var filtered []string
	for _, attr := range strings.Split(attrsStr, ":") {
		if len(attr) != 0 {
			filtered = append(filtered, attr)
		}
	}
	return roachpb.Attributes{Attrs: filtered}
}
