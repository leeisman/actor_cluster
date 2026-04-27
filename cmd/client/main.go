package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/frankieli/actor_cluster/pkg/discovery"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const defaultFlushDelay = 5 * time.Millisecond

type clientConfig struct {
	etcdEndpoints string
	nodePrefix    string
	batchSize     int
	flushDelay    time.Duration
}

var reqIDGenerator atomic.Uint64

func init() {
	// request_id 由上游入口產生，必須在整個叢集範圍內全域唯一。
	// 這裡保留無鎖的 machine-id + sequence 寫法，避免多個 client instance 撞號。
	rand.Seed(time.Now().UnixNano())
	machineID := uint64(rand.Intn(65536)) << 48
	reqIDGenerator.Store(machineID)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "stress":
		err = runStressCommand(os.Args[2:])
	case "serve":
		err = runServeCommand(os.Args[2:])
	case "-h", "--help", "help":
		printUsage()
		return
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func registerSharedFlags(fs *flag.FlagSet, cfg *clientConfig) {
	fs.StringVar(&cfg.etcdEndpoints, "etcd", "127.0.0.1:2379", "comma-separated etcd endpoints")
	fs.StringVar(&cfg.nodePrefix, "node-prefix", "/actor_cluster/nodes", "etcd key prefix")
	fs.IntVar(&cfg.batchSize, "batch", 2000, "max batch size for sending")
	fs.DurationVar(&cfg.flushDelay, "flush-delay", defaultFlushDelay, "max time to wait before flushing a partially filled batch")
}

func newRouterFromConfig(ctx context.Context, cfg clientConfig) (*Router, func(), error) {
	endpoints := strings.Split(cfg.etcdEndpoints, ",")
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("etcd connection failed: %w", err)
	}

	resolver := discovery.NewEtcdResolver(etcdCli, cfg.nodePrefix)
	if err := resolver.Watch(ctx); err != nil {
		etcdCli.Close()
		return nil, nil, fmt.Errorf("resolver watch failed: %w", err)
	}

	router := NewRouter(resolver, cfg.batchSize, cfg.flushDelay)
	cleanup := func() {
		router.StopAll()
		etcdCli.Close()
	}
	return router, cleanup, nil
}

func nextRequestID() uint64 {
	return reqIDGenerator.Add(1)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: %s <stress|serve> [flags]\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  stress   Run the original CLI load generator")
	fmt.Fprintln(os.Stderr, "  serve    Start the test web gateway server")
}
