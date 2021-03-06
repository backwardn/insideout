package main

import (
	"context"
	"encoding/json"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	// _ "net/http/pprof"

	log "github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_opentracing "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/namsral/flag"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metrics "github.com/slok/go-http-metrics/metrics/prometheus"
	"github.com/slok/go-http-metrics/middleware"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	"github.com/akhenakh/insideout"
	"github.com/akhenakh/insideout/insidesvc"
	"github.com/akhenakh/insideout/loglevel"
	"github.com/akhenakh/insideout/server"
	"github.com/akhenakh/insideout/server/debug"
	"github.com/akhenakh/insideout/storage/bbolt"
)

const appName = "insided"

var (
	version = "no version from LDFLAGS"

	logLevel        = flag.String("logLevel", "INFO", "DEBUG|INFO|WARN|ERROR")
	cacheCount      = flag.Int("cacheCount", 200, "Features count to cache, 0 to disable the cache")
	dbPath          = flag.String("dbPath", "inside.db", "Database path")
	httpMetricsPort = flag.Int("httpMetricsPort", 8088, "http port")
	httpAPIPort     = flag.Int("httpAPIPort", 8080, "http API port")
	grpcPort        = flag.Int("grpcPort", 9200, "gRPC API port")
	healthPort      = flag.Int("healthPort", 6666, "grpc health port")

	stopOnFirstFound = flag.Bool("stopOnFirstFound", false, "Stop in first feature found")
	strategy         = flag.String("strategy", insideout.DBStrategy, "Strategy to use: insidetree|shapeindex|db|postgis")

	httpServer        *http.Server
	grpcHealthServer  *grpc.Server
	grpcServer        *grpc.Server
	httpMetricsServer *http.Server
)

func main() {
	flag.Parse()

	logger := log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	logger = log.With(logger, "caller", log.Caller(5), "ts", log.DefaultTimestampUTC)
	logger = log.With(logger, "app", appName)
	logger = loglevel.NewLevelFilterFromString(logger, *logLevel)

	stdlog.SetOutput(log.NewStdlibAdapter(logger))

	switch *strategy {
	case insideout.InsideTreeStrategy, insideout.DBStrategy, insideout.ShapeIndexStrategy:
	default:
		level.Error(logger).Log("msg", "unknown strategy", "strategy", *strategy)
		os.Exit(2)
	}

	level.Info(logger).Log("msg", "Starting app", "version", version)

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	// catch termination
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupt)

	g, ctx := errgroup.WithContext(ctx)

	// pprof
	// go func() {
	// 	stdlog.Println(http.ListenAndServe("localhost:6060", nil))
	// }()

	storage, clean, err := bbolt.NewROStorage(*dbPath, logger)
	if err != nil {
		level.Error(logger).Log("msg", "failed to open storage", "error", err, "db_path", *dbPath)
		os.Exit(2)
	}
	defer clean()

	infos, err := storage.LoadIndexInfos()
	if err != nil {
		level.Error(logger).Log("msg", "failed to read infos", "error", err)
		os.Exit(2)
	}

	// gRPC Health Server
	healthServer := health.NewServer()
	g.Go(func() error {
		grpcHealthServer = grpc.NewServer()

		healthpb.RegisterHealthServer(grpcHealthServer, healthServer)

		haddr := fmt.Sprintf(":%d", *healthPort)
		hln, err := net.Listen("tcp", haddr)
		if err != nil {
			level.Error(logger).Log("msg", "gRPC Health server: failed to listen", "error", err)
			os.Exit(2)
		}
		level.Info(logger).Log("msg", fmt.Sprintf("gRPC health server listening at %s", haddr))
		return grpcHealthServer.Serve(hln)
	})

	// server
	server, err := server.New(storage, logger, healthServer,
		server.Options{
			StopOnFirstFound: *stopOnFirstFound,
			CacheCount:       *cacheCount,
			Strategy:         *strategy,
		})
	if err != nil {
		level.Error(logger).Log("msg", "can't get a working server", "error", err)
		os.Exit(2)
	}

	// web server metrics
	g.Go(func() error {
		httpMetricsServer = &http.Server{
			Addr:         fmt.Sprintf(":%d", *httpMetricsPort),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
		level.Info(logger).Log("msg", fmt.Sprintf("HTTP Metrics server listening at :%d", *httpMetricsPort))

		versionGauge.WithLabelValues(version).Add(1)
		dataVersionGauge.WithLabelValues(
			fmt.Sprintf("%s %s", infos.Filename, infos.IndexTime.Format(time.RFC3339)),
		).Add(1)

		// Register Prometheus metrics handler.
		http.Handle("/metrics", promhttp.Handler())

		if err := httpMetricsServer.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}

		return nil
	})

	// gRPC server
	g.Go(func() error {
		addr := fmt.Sprintf(":%d", *grpcPort)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			level.Error(logger).Log("msg", "gRPC server: failed to listen", "error", err)
			os.Exit(2)
		}

		grpc_prometheus.EnableHandlingTimeHistogram()

		grpcServer = grpc.NewServer(
			// MaxConnectionAge is just to avoid long connection, to facilitate load balancing
			// MaxConnectionAgeGrace will torn them, default to infinity
			grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: 5 * time.Minute}),
			grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
				grpc_opentracing.StreamServerInterceptor(),
				grpc_prometheus.StreamServerInterceptor,
			)),
			grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
				grpc_opentracing.UnaryServerInterceptor(),
				grpc_prometheus.UnaryServerInterceptor,
			)),
		)
		insidesvc.RegisterInsideServer(grpcServer, server)

		return grpcServer.Serve(ln)
	})

	// API web server
	g.Go(func() error {
		// metrics middleware.
		metricsMwr := middleware.New(middleware.Config{
			Recorder: metrics.NewRecorder(metrics.Config{Prefix: appName}),
		})

		r := mux.NewRouter()

		r.HandleFunc("/debug/cells", debug.S2CellQueryHandler)
		r.HandleFunc("/debug/get/{fid}/{loop_index}", server.DebugGetHandler)

		// serving static files
		r.PathPrefix("/debug/").Handler(http.StripPrefix("/debug/", http.FileServer(http.Dir("./static"))))

		// within API handler
		r.Handle("/api/within/{lat}/{lng}",
			handlers.CompressHandler(metricsMwr.Handler("/api/within/lat/lng",
				http.HandlerFunc(server.WithinHandler))))

		r.HandleFunc("/healthz", func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
			defer cancel()

			resp, err := healthServer.Check(ctx, &healthpb.HealthCheckRequest{
				Service: fmt.Sprintf("grpc.health.v1.%s", appName)},
			)
			if err != nil {
				json := []byte(fmt.Sprintf("{\"status\": \"%s\"}", healthpb.HealthCheckResponse_UNKNOWN.String()))
				w.WriteHeader(http.StatusInternalServerError)
				w.Write(json)
				return
			}
			if resp.Status != healthpb.HealthCheckResponse_SERVING {
				w.WriteHeader(http.StatusInternalServerError)
			}
			json := []byte(fmt.Sprintf("{\"status\": \"%s\"}", resp.Status.String()))
			w.Write(json)
		})

		r.HandleFunc("/version", func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			m := map[string]interface{}{"version": version, "infos": infos}
			b, _ := json.Marshal(m)
			w.Write(b)
		})

		httpServer = &http.Server{
			Addr:         fmt.Sprintf(":%d", *httpAPIPort),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			Handler:      handlers.CORS()(r),
		}
		level.Info(logger).Log("msg", fmt.Sprintf("HTTP API server listening at :%d", *httpAPIPort))

		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}

		return nil
	})

	level.Info(logger).Log("msg", "read index_infos", "feature_count", infos.FeatureCount)

	//TODO: perform a query first for shapeindex to be ready

	healthServer.SetServingStatus(fmt.Sprintf("grpc.health.v1.%s", appName), healthpb.HealthCheckResponse_SERVING)
	level.Info(logger).Log("msg", "serving status to SERVING")

	select {
	case <-interrupt:
		cancel()
		break
	case <-ctx.Done():
		break
	}

	level.Warn(logger).Log("msg", "received shutdown signal")

	healthServer.SetServingStatus(fmt.Sprintf("grpc.health.v1.%s", appName), healthpb.HealthCheckResponse_NOT_SERVING)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if httpMetricsServer != nil {
		_ = httpMetricsServer.Shutdown(shutdownCtx)
	}

	if httpServer != nil {
		_ = httpServer.Shutdown(shutdownCtx)
	}

	if grpcServer != nil {
		grpcServer.GracefulStop()
	}

	if grpcHealthServer != nil {
		grpcHealthServer.GracefulStop()
	}

	err = g.Wait()
	if err != nil {
		level.Error(logger).Log("msg", "server returning an error", "error", err)
		os.Exit(2)
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	fmt.Printf("Alloc = %v MiB", bToMb(m.Alloc))
	fmt.Printf("\tTotalAlloc = %v MiB", bToMb(m.TotalAlloc))
	fmt.Printf("\tSys = %v MiB", bToMb(m.Sys))
	fmt.Printf("\tNumGC = %v\n", m.NumGC)
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}
