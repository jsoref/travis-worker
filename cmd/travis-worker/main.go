package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/Sirupsen/logrus/hooks/sentry"
	"github.com/codegangsta/cli"
	"github.com/rcrowley/go-metrics"
	"github.com/rcrowley/go-metrics/librato"
	"github.com/streadway/amqp"
	"github.com/travis-ci/worker"
	"github.com/travis-ci/worker/backend"
	"github.com/travis-ci/worker/context"
	gocontext "golang.org/x/net/context"
)

func main() {
	app := cli.NewApp()
	app.Usage = "Travis Worker daemon"
	app.Version = worker.VersionString
	app.Author = "Travis CI GmbH"
	app.Email = "contact+travis-worker@travis-ci.org"

	app.Flags = worker.Flags
	app.Action = runWorker

	app.Run(os.Args)
}

func runWorker(c *cli.Context) {
	ctx, cancel := gocontext.WithCancel(gocontext.Background())
	logger := context.LoggerFromContext(ctx)

	logrus.SetFormatter(&logrus.TextFormatter{DisableColors: true})

	if c.String("pprof-port") != "" {
		// Start net/http/pprof server
		go func() {
			http.ListenAndServe(fmt.Sprintf("localhost:%s", c.String("pprof-port")), nil)
		}()
	}

	if c.Bool("debug") {
		logrus.SetLevel(logrus.DebugLevel)
	}

	logger.Info("worker started")
	defer logger.Info("worker finished")

	config := worker.ConfigFromCLIContext(c)

	logger.WithField("config", fmt.Sprintf("%+v", config)).Debug("read config")

	if config.SentryDSN != "" {
		sentryHook, err := logrus_sentry.NewSentryHook(config.SentryDSN, []logrus.Level{logrus.PanicLevel, logrus.FatalLevel, logrus.ErrorLevel})
		if err != nil {
			context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't create sentry hook")
		}

		logrus.AddHook(sentryHook)
	}

	if config.LibratoEmail != "" && config.LibratoToken != "" && config.LibratoSource != "" {
		context.LoggerFromContext(ctx).Info("starting librato metrics reporter")
		go librato.Librato(metrics.DefaultRegistry, time.Minute, config.LibratoEmail, config.LibratoToken, config.LibratoSource, []float64{0.95}, time.Millisecond)
	} else {
		context.LoggerFromContext(ctx).Info("starting logger metrics reporter")
		go metrics.Log(metrics.DefaultRegistry, time.Minute, log.New(os.Stderr, "metrics: ", log.Lmicroseconds))
	}

	amqpConn, err := amqp.Dial(config.AmqpURI)
	if err != nil {
		context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't connect to AMQP")
		return
	}

	go func() {
		errChan := make(chan *amqp.Error)
		errChan = amqpConn.NotifyClose(errChan)

		err, ok := <-errChan
		if ok {
			context.LoggerFromContext(ctx).WithField("err", err).Error("amqp connection errored, terminating")
			cancel()
		}
	}()

	context.LoggerFromContext(ctx).Debug("connected to AMQP")

	generator := worker.NewBuildScriptGenerator(config)
	provider, err := backend.NewProvider(config.ProviderName, ProviderConfigFromEnviron(config.ProviderName))
	if err != nil {
		context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't create backend provider")
		return
	}

	context.LoggerFromContext(ctx).WithFields(logrus.Fields{
		"provider": provider,
	}).Debug("built provider")

	commandDispatcher := worker.NewCommandDispatcher(ctx, amqpConn)
	go commandDispatcher.Run()

	pool := worker.NewProcessorPool(config.Hostname, ctx, config.HardTimeout, amqpConn,
		provider, generator, commandDispatcher)

	pool.SkipShutdownOnLogTimeout = config.SkipShutdownOnLogTimeout

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-signalChan
		if sig == syscall.SIGINT {
			context.LoggerFromContext(ctx).Info("SIGTERM received, starting graceful shutdown")
			pool.GracefulShutdown()
		} else {
			context.LoggerFromContext(ctx).Info("SIGINT received, shutting down immediately")
			cancel()
		}
	}()

	pool.Run(config.PoolSize, config.QueueName)

	err = amqpConn.Close()
	if err != nil {
		context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't close AMQP connection cleanly")
		return
	}
}

func ProviderConfigFromEnviron(providerName string) map[string]string {
	prefix := "TRAVIS_WORKER_" + strings.ToUpper(providerName) + "_"

	config := make(map[string]string)

	for _, e := range os.Environ() {
		if strings.HasPrefix(e, prefix) {
			pair := strings.SplitN(e, "=", 2)
			key := strings.ToLower(strings.TrimPrefix(pair[0], prefix))

			config[key] = pair[1]
		}
	}

	return config
}
