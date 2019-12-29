package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"time"

	jcr "github.com/DisposaBoy/JsonConfigReader"
	"github.com/unit-io/trace/broker"
	"github.com/unit-io/trace/config"
	"github.com/unit-io/trace/pkg/log"
	"github.com/rs/zerolog"
)

func main() {
	// Get the directory of the process
	exe, err := os.Executable()
	if err != nil {
		panic(err.Error())
	}

	var configfile = flag.String("config", "trace.conf", "Path to config file.")
	var listenOn = flag.String("listen", "", "Override address and port to listen on for HTTP(S) clients.")
	var clusterSelf = flag.String("cluster_self", "", "Override the name of the current cluster node")
	var varzPath = flag.String("varz", "/varz", "Expose runtime stats at the given endpoint, e.g. /varz. Disabled if not set")
	flag.Parse()

	// Default level for is fatal, unless debug flag is present
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	//*configfile = toAbsolutePath(rootpath, *configfile)
	*configfile = filepath.Join(filepath.Dir(exe), *configfile)
	log.Debug("main", "Using config from "+*configfile)
	var cfg *config.Config
	if file, err := os.Open(*configfile); err != nil {
		log.Fatal("main", "Failed to read config file", err)
	} else if err = json.NewDecoder(jcr.New(file)).Decode(&cfg); err != nil {
		log.Fatal("main", "Failed to parse config file", err)
	}

	zerolog.DurationFieldUnit = time.Nanosecond
	if cfg.LoggingLevel != "" {
		l := log.ParseLevel(cfg.LoggingLevel, zerolog.InfoLevel)
		zerolog.SetGlobalLevel(l)
	}

	if *listenOn != "" {
		cfg.Listen = *listenOn
	}

	if *varzPath != "" {
		cfg.VarzPath = *varzPath
	}

	// Initialize cluster and receive calculated workerId.
	// Cluster won't be started here yet.
	broker.ClusterInit(cfg.Cluster, clusterSelf)

	broker.Globals.ConnCache = broker.NewConnCache()

	svc, err := broker.NewService(context.Background(), cfg)
	if err != nil {
		panic(err.Error())
	}

	// Start accepting cluster traffic.
	if broker.Globals.Cluster != nil {
		broker.Globals.Cluster.Start()
	}

	broker.Globals.Service = svc

	//Listen and serve
	svc.Listen()
	log.Info("main", "Service is runnig at port "+cfg.Listen)
}
