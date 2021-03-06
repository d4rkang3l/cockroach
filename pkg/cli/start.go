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

package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/elastic/gosigar"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/cli/cliflags"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/grpcutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/logflags"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

// jemallocHeapDump is an optional function to be called at heap dump time.
// This will be non-nil when jemalloc is linked in with profiling enabled.
// The function takes a filename to write the profile to.
var jemallocHeapDump func(string) error

// startCmd starts a node by initializing the stores and joining
// the cluster.
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "start a node",
	Long: `
Start a CockroachDB node, which will export data from one or more
storage devices, specified via --store flags.

If no cluster exists yet and this is the first node, no additional
flags are required. If the cluster already exists, and this node is
uninitialized, specify the --join flag to point to any healthy node
(or list of nodes) already part of the cluster.
`,
	Example: `  cockroach start --insecure --store=attrs=ssd,path=/mnt/ssd1 [--join=host:port,[host:port]]`,
	RunE:    MaybeShoutError(MaybeDecorateGRPCError(runStart)),
}

// maxSizePerProfile is the maximum total size in bytes for profiles per
// profile type.
var maxSizePerProfile = envutil.EnvOrDefaultInt64(
	"COCKROACH_MAX_SIZE_PER_PROFILE", 100<<20 /* 100 MB */)

// gcProfiles removes old profiles matching the specified prefix when the sum
// of newer profiles is larger than maxSize. Requires that the suffix used for
// the profiles indicates age (e.g. by using a date/timestamp suffix) such that
// sorting the filenames corresponds to ordering the profiles from oldest to
// newest.
func gcProfiles(dir, prefix string, maxSize int64) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		log.Warning(context.Background(), err)
		return
	}
	var sum int64
	var found int
	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
		if !f.Mode().IsRegular() {
			continue
		}
		if !strings.HasPrefix(f.Name(), prefix) {
			continue
		}
		found++
		sum += f.Size()
		if found == 1 {
			// Always keep the most recent profile.
			continue
		}
		if sum <= maxSize {
			continue
		}
		if err := os.Remove(filepath.Join(dir, f.Name())); err != nil {
			log.Info(context.Background(), err)
		}
	}
}

func initMemProfile(ctx context.Context, dir string) {
	const jeprof = "jeprof."
	const memprof = "memprof."

	gcProfiles(dir, jeprof, maxSizePerProfile)
	gcProfiles(dir, memprof, maxSizePerProfile)

	memProfileInterval := envutil.EnvOrDefaultDuration("COCKROACH_MEMPROF_INTERVAL", -1)
	if memProfileInterval <= 0 {
		return
	}
	if min := time.Second; memProfileInterval < min {
		log.Infof(ctx, "fixing excessively short memory profiling interval: %s -> %s",
			memProfileInterval, min)
		memProfileInterval = min
	}

	if jemallocHeapDump != nil {
		log.Infof(ctx, "writing go and jemalloc memory profiles to %s every %s", dir, memProfileInterval)
	} else {
		log.Infof(ctx, "writing go only memory profiles to %s every %s", dir, memProfileInterval)
		log.Infof(ctx, `to enable jmalloc profiling: "export MALLOC_CONF=prof:true" or "ln -s prof:true /etc/malloc.conf"`)
	}

	go func() {
		ctx := context.Background()
		t := time.NewTicker(memProfileInterval)
		defer t.Stop()

		for {
			<-t.C

			func() {
				const format = "2006-01-02T15_04_05.999"
				suffix := timeutil.Now().Format(format)

				// Try jemalloc heap profile first, we only log errors.
				if jemallocHeapDump != nil {
					jepath := filepath.Join(dir, jeprof+suffix)
					if err := jemallocHeapDump(jepath); err != nil {
						log.Warningf(ctx, "error writing jemalloc heap %s: %s", jepath, err)
					}
					gcProfiles(dir, jeprof, maxSizePerProfile)
				}

				path := filepath.Join(dir, memprof+suffix)
				// Try writing a go heap profile.
				f, err := os.Create(path)
				if err != nil {
					log.Warningf(ctx, "error creating go heap file %s", err)
					return
				}
				defer f.Close()
				if err = pprof.WriteHeapProfile(f); err != nil {
					log.Warningf(ctx, "error writing go heap %s: %s", path, err)
					return
				}
				gcProfiles(dir, memprof, maxSizePerProfile)
			}()
		}
	}()
}

func initCPUProfile(ctx context.Context, dir string) {
	const cpuprof = "cpuprof."
	gcProfiles(dir, cpuprof, maxSizePerProfile)

	cpuProfileInterval := envutil.EnvOrDefaultDuration("COCKROACH_CPUPROF_INTERVAL", -1)
	if cpuProfileInterval <= 0 {
		return
	}
	if min := time.Second; cpuProfileInterval < min {
		log.Infof(ctx, "fixing excessively short cpu profiling interval: %s -> %s",
			cpuProfileInterval, min)
		cpuProfileInterval = min
	}

	go func() {
		defer log.RecoverAndReportPanic(ctx, &serverCfg.Settings.SV)

		ctx := context.Background()

		t := time.NewTicker(cpuProfileInterval)
		defer t.Stop()

		var currentProfile *os.File
		defer func() {
			if currentProfile != nil {
				pprof.StopCPUProfile()
				currentProfile.Close()
			}
		}()

		for {
			func() {
				const format = "2006-01-02T15_04_05.999"
				suffix := timeutil.Now().Add(cpuProfileInterval).Format(format)
				f, err := os.Create(filepath.Join(dir, cpuprof+suffix))
				if err != nil {
					log.Warningf(ctx, "error creating go cpu file %s", err)
					return
				}

				// Stop the current profile if it exists.
				if currentProfile != nil {
					pprof.StopCPUProfile()
					currentProfile.Close()
					currentProfile = nil
					gcProfiles(dir, cpuprof, maxSizePerProfile)
				}

				// Start the new profile.
				if err := pprof.StartCPUProfile(f); err != nil {
					log.Warningf(ctx, "unable to start cpu profile: %v", err)
					f.Close()
					return
				}
				currentProfile = f
			}()

			<-t.C
		}
	}()
}

func initBlockProfile() {
	// Enable the block profile for a sample of mutex and channel operations.
	// Smaller values provide more accurate profiles but are more
	// expensive. 0 and 1 are special: 0 disables the block profile and
	// 1 captures 100% of block events. For other values, the profiler
	// will sample one event per X nanoseconds spent blocking.
	//
	// The block profile can be viewed with `pprof http://HOST:PORT/debug/pprof/block`
	d := envutil.EnvOrDefaultInt64("COCKROACH_BLOCK_PROFILE_RATE",
		10000000 /* 1 sample per 10 milliseconds spent blocking */)
	runtime.SetBlockProfileRate(int(d))
}

type percentResolverFunc func(percent int) (int64, error)

// bytesOrPercentageValue is a flag that accepts an integer value, an integer
// plus a unit (e.g. 32GB or 32GiB) or a percentage (e.g. 32%). In all these
// cases, it transforms the string flag input into an int64 value.
//
// Since it accepts a percentage, instances need to be configured with
// instructions on how to resolve a percentage to a number (i.e. the answer to
// the question "a percentage of what?"). This is done by taking in a
// percentResolverFunc. There are predefined ones: memoryPercentResolver and
// diskPercentResolverFactory.
//
// bytesOrPercentageValue can be used in two ways:
// 1. Upon flag parsing, it can write an int64 value through a pointer specified
// by the caller.
// 2. It can store the flag value as a string and only convert it to an int64 on
// a subsequent Resolve() call. Input validation still happens at flag parsing
// time.
//
// Option 2 is useful when percentages cannot be resolved at flag parsing time.
// For example, we have flags that can be expressed as percentages of the
// capacity of storage device. Which storage device is in question might only be
// known once other flags are parsed (e.g. --max-disk-temp-storage=10% depends
// on --store).
type bytesOrPercentageValue struct {
	val  *int64
	bval *humanizeutil.BytesValue

	origVal string

	// percentResolver is used to turn a percent string into a value. See
	// memoryPercentResolver() and diskPercentResolverFactory().
	percentResolver percentResolverFunc
}

// memoryPercentResolver turns a percent into the respective fraction of the
// system's internal memory.
func memoryPercentResolver(percent int) (int64, error) {
	sizeBytes, err := server.GetTotalMemory(context.TODO())
	if err != nil {
		return 0, err
	}
	return (sizeBytes * int64(percent)) / 100, nil
}

// diskPercentResolverFactory takes in a path and produces a percentResolverFunc
// bound to the respective storage device.
//
// An error is returned if dir does not exist.
func diskPercentResolverFactory(dir string) (percentResolverFunc, error) {
	fileSystemUsage := gosigar.FileSystemUsage{}
	if err := fileSystemUsage.Get(dir); err != nil {
		return nil, err
	}
	if fileSystemUsage.Total > math.MaxInt64 {
		return nil, fmt.Errorf("unsupported disk size %s, max supported size is %s",
			humanize.IBytes(fileSystemUsage.Total), humanizeutil.IBytes(math.MaxInt64))
	}
	deviceCapacity := int64(fileSystemUsage.Total)

	return func(percent int) (int64, error) {
		return (deviceCapacity * int64(percent)) / 100, nil
	}, nil
}

// newBytesOrPercentageValue creates a bytesOrPercentageValue.
//
// v and percentResolver can be nil (either they're both specified or they're
// both nil). If they're nil, then Resolve() has to be called later to get the
// passed-in value.
func newBytesOrPercentageValue(
	v *int64, percentResolver func(percent int) (int64, error),
) *bytesOrPercentageValue {
	if v == nil {
		v = new(int64)
	}
	return &bytesOrPercentageValue{
		val:             v,
		bval:            humanizeutil.NewBytesValue(v),
		percentResolver: percentResolver,
	}
}

func (b *bytesOrPercentageValue) Set(s string) error {
	b.origVal = s
	if strings.HasSuffix(s, "%") {
		percent, err := strconv.Atoi(s[:len(s)-1])
		if err != nil {
			return err
		}
		if percent < 0 || percent > 99 {
			return fmt.Errorf("percentage %s out of range 0%% - 99%%", s)
		}

		if b.percentResolver == nil {
			// percentResolver not set means that this flag is not yet supposed to set
			// any value.
			return nil
		}

		absVal, err := b.percentResolver(percent)
		if err != nil {
			return err
		}
		s = fmt.Sprint(absVal)
	}
	return b.bval.Set(s)
}

// Resolve can be called to get the flag's value (if any). If the flag had been
// previously set, *v will be written.
func (b *bytesOrPercentageValue) Resolve(v *int64, percentResolver percentResolverFunc) error {
	// The flag was not passed on the command line.
	if b.origVal == "" {
		return nil
	}
	b.percentResolver = percentResolver
	b.val = v
	b.bval = humanizeutil.NewBytesValue(v)
	return b.Set(b.origVal)
}

func (b *bytesOrPercentageValue) Type() string {
	return b.bval.Type()
}

func (b *bytesOrPercentageValue) String() string {
	return b.bval.String()
}

func (b *bytesOrPercentageValue) IsSet() bool {
	return b.bval.IsSet()
}

var cacheSizeValue = newBytesOrPercentageValue(&serverCfg.CacheSize, memoryPercentResolver)
var sqlSizeValue = newBytesOrPercentageValue(&serverCfg.SQLMemoryPoolSize, memoryPercentResolver)
var diskTempStorageSizeValue = newBytesOrPercentageValue(nil /* v */, nil /* percentResolver */)

func initExternalIODir(ctx context.Context, firstStore base.StoreSpec) (string, error) {
	if externalIODir == "" && !firstStore.InMemory {
		externalIODir = filepath.Join(firstStore.Path, "extern")
	}
	if externalIODir == "" || externalIODir == "disabled" {
		return "", nil
	}
	if !filepath.IsAbs(externalIODir) {
		return "", errors.Errorf("%s path must be absolute", cliflags.ExternalIODir.Name)
	}
	return externalIODir, nil
}

func initTempStorageConfig(
	ctx context.Context, firstStore base.StoreSpec,
) (base.TempStorageConfig, error) {
	var recordPath string
	if !firstStore.InMemory {
		recordPath = filepath.Join(firstStore.Path, server.TempDirsRecordFilename)
	}

	var err error
	// Need to first clean up any abandoned temporary directories from
	// the temporary directory record file before creating any new
	// temporary directories in case the disk is completely full.
	if recordPath != "" {
		if err = util.CleanupTempDirs(recordPath); err != nil {
			return base.TempStorageConfig{}, errors.Wrap(err, "could not cleanup temporary directories from record file")
		}
	}

	// The temp store size can depend on the location of the first regular store
	// (if it's expressed as a percentage), so we resolve that flag here.
	var tempStorePercentageResolver percentResolverFunc
	if !firstStore.InMemory {
		dir := firstStore.Path
		// Create the store dir, if it doesn't exist. The dir is required to exist
		// by diskPercentResolverFactory.
		if err = os.MkdirAll(dir, 0755); err != nil {
			return base.TempStorageConfig{}, errors.Wrapf(err, "failed to create dir for first store: %s", dir)
		}
		tempStorePercentageResolver, err = diskPercentResolverFactory(dir)
		if err != nil {
			return base.TempStorageConfig{}, errors.Wrapf(err, "failed to create resolver for: %s", dir)
		}
	} else {
		tempStorePercentageResolver = memoryPercentResolver
	}
	var tempStorageMaxSizeBytes int64
	if err = diskTempStorageSizeValue.Resolve(
		&tempStorageMaxSizeBytes, tempStorePercentageResolver,
	); err != nil {
		return base.TempStorageConfig{}, err
	}
	if !diskTempStorageSizeValue.IsSet() {
		// The default temp storage size is different when the temp
		// storage is in memory (which occurs when no temp directory
		// is specified and the first store is in memory).
		if tempDir == "" && firstStore.InMemory {
			tempStorageMaxSizeBytes = base.DefaultInMemTempStorageMaxSizeBytes
		} else {
			tempStorageMaxSizeBytes = base.DefaultTempStorageMaxSizeBytes
		}
	}

	// Initialize a base.TempStorageConfig based on first store's spec and
	// cli flags.
	tempStorageConfig := base.TempStorageConfigFromEnv(
		ctx,
		firstStore,
		tempDir,
		tempStorageMaxSizeBytes,
	)

	// Set temp directory to first store's path if the temp storage is not
	// in memory.
	if tempDir == "" && !tempStorageConfig.InMemory {
		tempDir = firstStore.Path
	}
	// Create the temporary subdirectory for the temp engine.
	if tempStorageConfig.Path, err = util.CreateTempDir(tempDir, server.TempDirPrefix); err != nil {
		return base.TempStorageConfig{}, errors.Wrap(err, "could not create temporary directory for temp storage")
	}

	// We record the new temporary directory in the record file (if it
	// exists) for cleanup in case the node crashes.
	if recordPath != "" {
		if err = util.RecordTempDir(recordPath, tempStorageConfig.Path); err != nil {
			return base.TempStorageConfig{}, errors.Wrapf(
				err,
				"could not record temporary directory path to record file: %s",
				recordPath,
			)
		}
	}

	return tempStorageConfig, nil
}

// runStart starts the cockroach node using --store as the list of
// storage devices ("stores") on this machine and --join as the list
// of other active nodes used to join this node to the cockroach
// cluster, if this is its first time connecting.
func runStart(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageAndError(cmd)
	}
	tBegin := timeutil.Now()

	if ok, err := maybeRerunBackground(); ok {
		return err
	}

	// Deal with flags that may depend on other flags.

	tracer := serverCfg.Settings.Tracer
	sp := tracer.StartSpan("server start")
	ctx := opentracing.ContextWithSpan(context.Background(), sp)

	var err error
	if serverCfg.TempStorageConfig, err = initTempStorageConfig(ctx, serverCfg.Stores.Specs[0]); err != nil {
		return err
	}
	if serverCfg.Settings.ExternalIODir, err = initExternalIODir(ctx, serverCfg.Stores.Specs[0]); err != nil {
		return err
	}

	// Use the server-specific values for some flags and settings.
	serverCfg.Insecure = startCtx.serverInsecure
	serverCfg.SSLCertsDir = startCtx.serverSSLCertsDir
	serverCfg.User = security.NodeUser

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	// Set up the logging and profiling output.
	// It is important that no logging occurs before this point or the log files
	// will be created in $TMPDIR instead of their expected location.
	stopper, err := setupAndInitializeLoggingAndProfiling(ctx)
	if err != nil {
		return err
	}

	serverCfg.Report(ctx)

	// Run the rest of the startup process in the background to avoid preventing
	// proper handling of signals if we get stuck on something during
	// initialization (#10138).
	var serverStatusMu struct {
		syncutil.Mutex
		// Used to synchronize server startup with server shutdown if something
		// interrupts the process during initialization (it isn't safe to try to
		// drain a server that doesn't exist or is in the middle of starting up,
		// or to start a server after draining has begun).
		started, draining bool
	}
	var s *server.Server
	errChan := make(chan error, 1)
	go func() {
		// Ensure that the log files see the startup messages immediately.
		defer log.Flush()
		defer func() {
			if s != nil {
				log.RecoverAndReportPanic(ctx, &s.ClusterSettings().SV)
			}
		}()
		defer sp.Finish()
		if err := func() error {
			if err := serverCfg.InitNode(); err != nil {
				return errors.Wrap(err, "failed to initialize node")
			}

			log.Info(ctx, "starting cockroach node")
			if envVarsUsed := envutil.GetEnvVarsUsed(); len(envVarsUsed) > 0 {
				log.Infof(ctx, "using local environment variables: %s", strings.Join(envVarsUsed, ", "))
			}

			var err error
			s, err = server.NewServer(serverCfg, stopper)
			if err != nil {
				return errors.Wrap(err, "failed to start server")
			}

			serverStatusMu.Lock()
			draining := serverStatusMu.draining
			serverStatusMu.Unlock()
			if draining {
				return nil
			}

			if err := s.Start(ctx); err != nil {
				if le, ok := err.(server.ListenError); ok {
					const errorPrefix = "consider changing the port via --"
					if le.Addr == serverCfg.Addr {
						err = errors.Wrap(err, errorPrefix+cliflags.ServerPort.Name)
					} else if le.Addr == serverCfg.HTTPAddr {
						err = errors.Wrap(err, errorPrefix+cliflags.ServerHTTPPort.Name)
					}
				}

				return errors.Wrap(err, "cockroach server exited with error")
			}

			serverStatusMu.Lock()
			serverStatusMu.started = true
			serverStatusMu.Unlock()

			// We don't do this in (*server.Server).Start() because we don't want it
			// in tests.
			if !envutil.EnvOrDefaultBool("COCKROACH_SKIP_UPDATE_CHECK", false) {
				s.PeriodicallyCheckForUpdates()
			}

			pgURL, err := serverCfg.PGURL(url.User(sqlConnUser))
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			info := build.GetInfo()
			tw := tabwriter.NewWriter(&buf, 2, 1, 2, ' ', 0)
			fmt.Fprintf(tw, "CockroachDB node starting at %s (took %0.1fs)\n", timeutil.Now(), timeutil.Since(tBegin).Seconds())
			fmt.Fprintf(tw, "build:\t%s %s @ %s (%s)\n", info.Distribution, info.Tag, info.Time, info.GoVersion)
			fmt.Fprintf(tw, "admin:\t%s\n", serverCfg.AdminURL())
			fmt.Fprintf(tw, "sql:\t%s\n", pgURL)
			if len(serverCfg.SocketFile) != 0 {
				fmt.Fprintf(tw, "socket:\t%s\n", serverCfg.SocketFile)
			}
			fmt.Fprintf(tw, "logs:\t%s\n", flag.Lookup("log-dir").Value)
			if serverCfg.Attrs != "" {
				fmt.Fprintf(tw, "attrs:\t%s\n", serverCfg.Attrs)
			}
			if len(serverCfg.Locality.Tiers) > 0 {
				fmt.Fprintf(tw, "locality:\t%s\n", serverCfg.Locality)
			}
			if s.TempDir() != "" {
				fmt.Fprintf(tw, "temp dir:\t%s\n", s.TempDir())
			}
			if ext := s.ClusterSettings().ExternalIODir; ext != "" {
				fmt.Fprintf(tw, "external I/O path: \t%s\n", ext)
			} else {
				fmt.Fprintf(tw, "external I/O path: \t<disabled>\n")
			}
			for i, spec := range serverCfg.Stores.Specs {
				fmt.Fprintf(tw, "store[%d]:\t%s\n", i, spec)
			}
			initialBoot := s.InitialBoot()
			nodeID := s.NodeID()
			if initialBoot {
				if nodeID == server.FirstNodeID {
					fmt.Fprintf(tw, "status:\tinitialized new cluster\n")
				} else {
					fmt.Fprintf(tw, "status:\tinitialized new node, joined pre-existing cluster\n")
				}
			} else {
				fmt.Fprintf(tw, "status:\trestarted pre-existing node\n")
			}
			fmt.Fprintf(tw, "clusterID:\t%s\n", s.ClusterID())
			fmt.Fprintf(tw, "nodeID:\t%d\n", nodeID)
			if err := tw.Flush(); err != nil {
				return err
			}
			msg := buf.String()
			log.Infof(ctx, "node startup completed:\n%s", msg)
			if !log.LoggingToStderr(log.Severity_INFO) {
				fmt.Print(msg)
			}
			return nil
		}(); err != nil {
			errChan <- err
		}
	}()

	shutdownSpan := tracer.StartSpan("server shutdown")
	defer shutdownSpan.Finish()
	shutdownCtx := opentracing.ContextWithSpan(context.Background(), shutdownSpan)

	var returnErr error

	stopWithoutDrain := make(chan struct{}) // closed if interrupted very early

	// Block until one of the signals above is received or the stopper
	// is stopped externally (for example, via the quit endpoint).
	select {
	case err := <-errChan:
		// SetSync both flushes and ensures that subsequent log writes are flushed too.
		log.SetSync(true)
		return err
	case <-stopper.ShouldStop():
		// Server is being stopped externally and our job is finished
		// here since we don't know if it's a graceful shutdown or not.
		<-stopper.IsStopped()
		// SetSync both flushes and ensures that subsequent log writes are flushed too.
		log.SetSync(true)
		return nil
	case sig := <-signalCh:
		// We start synchronizing log writes from here, because if a
		// signal was received there is a non-zero chance the sender of
		// this signal will follow up with SIGKILL if the shutdown is not
		// timely, and we don't want logs to be lost.
		log.SetSync(true)
		log.Infof(shutdownCtx, "received signal '%s'", sig)
		if sig == os.Interrupt {
			// Graceful shutdown after an interrupt should cause the process
			// to terminate with a non-zero exit code; however SIGTERM is
			// "legitimate" and should be acknowledged with a success exit
			// code. So we keep the error state here for later.
			returnErr = &cliError{
				exitCode: 1,
				// INFO because a single interrupt is rather innocuous.
				severity: log.Severity_INFO,
				cause:    errors.New("interrupted"),
			}
			msgDouble := "Note: a second interrupt will skip graceful shutdown and terminate forcefully"
			fmt.Fprintln(os.Stdout, msgDouble)
		}
		go func() {
			serverStatusMu.Lock()
			serverStatusMu.draining = true
			drainingIsSafe := serverStatusMu.started
			serverStatusMu.Unlock()

			// drainingIsSafe may have been set in the meantime, but that's ok.
			// In the worst case, we're not draining a Server that has *just*
			// started. Not desirable, but not terrible either.
			if !drainingIsSafe {
				close(stopWithoutDrain)
				return
			}
			if _, err := s.Drain(server.GracefulDrainModes); err != nil {
				// Don't use shutdownCtx because this is in a goroutine that may
				// still be running after shutdownCtx's span has been finished.
				log.Warning(context.Background(), err)
			}
			stopper.Stop(context.Background())
		}()
	}

	const msgDrain = "initiating graceful shutdown of server"
	log.Info(shutdownCtx, msgDrain)
	fmt.Fprintln(os.Stdout, msgDrain)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				log.Infof(context.Background(), "%d running tasks", stopper.NumTasks())
			case <-stopper.ShouldStop():
				return
			case <-stopWithoutDrain:
				return
			}
		}
	}()

	const hardShutdownHint = " - node may take longer to restart & clients may need to wait for leases to expire"
	select {
	case sig := <-signalCh:
		// This new signal is not welcome, as it interferes with the graceful
		// shutdown process. On Unix, a signal that was not handled gracefully by
		// the application should be visible to other processes as an exit code
		// encoded as 128+signal number.
		//
		// Also, on Unix, os.Signal is syscall.Signal and it's convertible to int.
		returnErr = &cliError{
			exitCode: 128 + int(sig.(syscall.Signal)),
			severity: log.Severity_ERROR,
			cause: errors.Errorf(
				"received signal '%s' during shutdown, initiating hard shutdown%s", sig, hardShutdownHint),
		}
		// NB: we do not return here to go through log.Flush below.
	case <-time.After(time.Minute):
		returnErr = errors.Errorf("time limit reached, initiating hard shutdown%s", hardShutdownHint)
		// NB: we do not return here to go through log.Flush below.
	case <-stopper.IsStopped():
		const msgDone = "server drained and shutdown completed"
		log.Infof(shutdownCtx, msgDone)
		fmt.Fprintln(os.Stdout, msgDone)
	case <-stopWithoutDrain:
		const msgDone = "too early to drain; used hard shutdown instead"
		log.Infof(shutdownCtx, msgDone)
		fmt.Fprintln(os.Stdout, msgDone)
	}

	return returnErr
}

func maybeWarnCacheSize() {
	if cacheSizeValue.IsSet() {
		return
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Using the default setting for --cache (%s).\n", cacheSizeValue)
	fmt.Fprintf(&buf, "  A significantly larger value is usually needed for good performance.\n")
	if size, err := server.GetTotalMemory(context.Background()); err == nil {
		fmt.Fprintf(&buf, "  If you have a dedicated server a reasonable setting is --cache=25%% (%s).",
			humanizeutil.IBytes(size/4))
	} else {
		fmt.Fprintf(&buf, "  If you have a dedicated server a reasonable setting is 25%% of physical memory.")
	}
	log.Warning(context.Background(), buf.String())
}

// setupAndInitializeLoggingAndProfiling does what it says on the label.
// Prior to this however it determines suitable defaults for the
// logging output directory and the verbosity level of stderr logging.
// We only do this for the "start" command which is why this work
// occurs here and not in an OnInitialize function.
func setupAndInitializeLoggingAndProfiling(ctx context.Context) (*stop.Stopper, error) {
	// Default the log directory to the "logs" subdirectory of the first
	// non-memory store. If more than one non-memory stores is detected,
	// print a warning.
	ambiguousLogDirs := false
	pf := cockroachCmd.PersistentFlags()
	f := pf.Lookup(logflags.LogDirName)
	if !log.DirSet() && !f.Changed {
		// We only override the log directory if the user has not explicitly
		// disabled file logging using --log-dir="".
		newDir := ""
		for _, spec := range serverCfg.Stores.Specs {
			if spec.InMemory {
				continue
			}
			if newDir != "" {
				ambiguousLogDirs = true
				break
			}
			newDir = filepath.Join(spec.Path, "logs")
		}
		if err := f.Value.Set(newDir); err != nil {
			return nil, err
		}
	}

	// We need an output directory below. We use the current directory unless
	// the log directory is configured, in which case we use that.
	outputDirectory := "."
	if logDir := f.Value.String(); logDir != "" {
		outputDirectory = logDir

		ls := pf.Lookup(logflags.LogToStderrName)
		if !ls.Changed {
			// Unless the settings were overridden by the user, silence
			// logging to stderr because the messages will go to a log file.
			if err := ls.Value.Set(log.Severity_NONE.String()); err != nil {
				return nil, err
			}
		}

		// Make sure the path exists.
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, err
		}
		log.Eventf(ctx, "created log directory %s", logDir)

		// Start the log file GC daemon to remove files that make the log
		// directory too large.
		log.StartGCDaemon()
	}

	if ambiguousLogDirs {
		// Note that we can't report this message earlier, because the log directory
		// may not have been ready before the call to MkdirAll() above.
		log.Shout(ctx, log.Severity_WARNING, "multiple stores configured"+
			" and --log-dir not specified, you may want to specify --log-dir to disambiguate.")
	}

	if startCtx.serverInsecure {
		// Use a non-annotated context here since the annotation just looks funny,
		// particularly to new users (made worse by it always printing as [n?]).
		addr := serverConnHost
		if addr == "" {
			addr = "<all your IP addresses>"
		}
		log.Shout(context.Background(), log.Severity_WARNING,
			"RUNNING IN INSECURE MODE!\n\n"+
				"- Your cluster is open for any client that can access "+addr+".\n"+
				"- Any user, even root, can log in without providing a password.\n"+
				"- Any user, connecting as root, can read or write any data in your cluster.\n"+
				"- There is no network encryption nor authentication, and thus no confidentiality.\n\n"+
				"Check out how to secure your cluster: "+base.DocsURL("secure-a-cluster.html"))
	}

	maybeWarnCacheSize()

	// We log build information to stdout (for the short summary), but also
	// to stderr to coincide with the full logs.
	info := build.GetInfo()
	log.Infof(ctx, info.Short())

	initMemProfile(ctx, outputDirectory)
	initCPUProfile(ctx, outputDirectory)
	initBlockProfile()

	// Disable Stopper task tracking as performing that call site tracking is
	// moderately expensive (certainly outweighing the infrequent benefit it
	// provides).
	stopper := initBacktrace(outputDirectory)
	log.Event(ctx, "initialized profiles")

	return stopper, nil
}

func addrWithDefaultHost(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port), nil
}

func getClientGRPCConn() (*grpc.ClientConn, *hlc.Clock, *stop.Stopper, error) {
	// 0 to disable max offset checks; this RPC context is not a member of the
	// cluster, so there's no need to enforce that its max offset is the same
	// as that of nodes in the cluster.
	clock := hlc.NewClock(hlc.UnixNano, 0)
	stopper := stop.NewStopper()
	rpcContext := rpc.NewContext(
		log.AmbientContext{Tracer: serverCfg.Settings.Tracer},
		serverCfg.Config,
		clock,
		stopper,
	)
	addr, err := addrWithDefaultHost(serverCfg.AdvertiseAddr)
	if err != nil {
		return nil, nil, nil, err
	}
	conn, err := rpcContext.GRPCDial(addr)
	if err != nil {
		return nil, nil, nil, err
	}
	return conn, clock, stopper, nil
}

func getAdminClient() (serverpb.AdminClient, *stop.Stopper, error) {
	conn, _, stopper, err := getClientGRPCConn()
	if err != nil {
		return nil, nil, err
	}
	return serverpb.NewAdminClient(conn), stopper, nil
}

func stopperContext(stopper *stop.Stopper) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	stopper.AddCloser(stop.CloserFn(cancel))
	return ctx
}

// quitCmd command shuts down the node server.
var quitCmd = &cobra.Command{
	Use:   "quit",
	Short: "drain and shutdown node\n",
	Long: `
Shutdown the server. The first stage is drain, where any new requests
will be ignored by the server. When all extant requests have been
completed, the server exits.
`,
	RunE: MaybeDecorateGRPCError(runQuit),
}

// checkNodeRunning performs a no-op RPC and returns an error if it failed to
// connect to the server.
func checkNodeRunning(ctx context.Context, c serverpb.AdminClient) error {
	// Send a no-op Drain request.
	stream, err := c.Drain(ctx, &serverpb.DrainRequest{
		On:       nil,
		Shutdown: false,
	})
	if err != nil {
		return errors.Wrap(err, "Failed to connect to the node: error sending drain request")
	}
	// Ignore errors from the stream. We've managed to connect to the node above,
	// and that's all that this function is interested in.
	for {
		if _, err := stream.Recv(); err != nil {
			if err != io.EOF {
				log.Warningf(ctx, "unexpected error from no-op Drain request: %s", err)
			}
			break
		}
	}
	return nil
}

// doShutdown attempts to trigger a server shutdown. When given an empty
// onModes slice, it's a hard shutdown.
//
// errTryHardShutdown is returned if the caller should do a hard-shutdown.
func doShutdown(ctx context.Context, c serverpb.AdminClient, onModes []int32) error {
	// We want to distinguish between the case in which we can't even connect to
	// the server (in which case we don't want our caller to try to come back with
	// a hard retry) and the case in which an attempt to shut down fails (times
	// out, or perhaps drops the connection while waiting). To that end, we first
	// run a noop DrainRequest. If that fails, we give up.
	if err := checkNodeRunning(ctx, c); err != nil {
		return err
	}
	// Send a drain request and continue reading until the connection drops (which
	// then counts as a success, for the connection dropping is likely the result
	// of the Stopper having reached the final stages of shutdown).
	stream, err := c.Drain(ctx, &serverpb.DrainRequest{
		On:       onModes,
		Shutdown: true,
	})
	if err != nil {
		//  This most likely means that we shut down successfully. Note that
		//  sometimes the connection can be shut down even before a DrainResponse gets
		//  sent back to us, so we don't require a response on the stream (see
		//  #14184).
		if grpcutil.IsClosedConnection(err) {
			return nil
		}
		return errors.Wrap(err, "Error sending drain request")
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if grpcutil.IsClosedConnection(err) {
				return nil
			}
			// Unexpected error; the caller should try again (and harder).
			return errTryHardShutdown{err}
		}
	}
}

type errTryHardShutdown struct{ error }

// runQuit accesses the quit shutdown path.
func runQuit(cmd *cobra.Command, args []string) (err error) {
	if len(args) != 0 {
		return usageAndError(cmd)
	}
	defer func() {
		if err == nil {
			fmt.Println("ok")
		}
	}()
	onModes := make([]int32, len(server.GracefulDrainModes))
	for i, m := range server.GracefulDrainModes {
		onModes[i] = int32(m)
	}

	c, stopper, err := getAdminClient()
	if err != nil {
		return err
	}
	ctx := stopperContext(stopper)
	defer stopper.Stop(ctx)

	if quitCtx.serverDecommission {
		var myself []string // will remain empty, which means target yourself
		if err := runDecommissionNodeImpl(ctx, c, nodeDecommissionWaitAll, myself); err != nil {
			return err
		}
	}
	errChan := make(chan error, 1)
	go func() {
		errChan <- doShutdown(ctx, c, onModes)
	}()
	select {
	case err := <-errChan:
		if err != nil {
			if _, ok := err.(errTryHardShutdown); ok {
				fmt.Printf("graceful shutdown failed: %s; proceeding with hard shutdown\n", err)
				break
			}
			return err
		}
		return nil
	case <-time.After(time.Minute):
		fmt.Println("timed out; proceeding with hard shutdown")
	}
	// Not passing drain modes tells the server to not bother and go
	// straight to shutdown.
	return errors.Wrap(doShutdown(ctx, c, nil), "hard shutdown failed")
}
