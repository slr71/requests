package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cyverse-de/requests/clients/notificationagent"

	"go.opentelemetry.io/contrib/instrumentation/github.com/labstack/echo/otelecho"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"

	"github.com/cyverse-de/requests/clients/iplantgroups"

	"github.com/cyverse-de/requests/clients/iplantemail"

	_ "github.com/lib/pq"

	"github.com/cyverse-de/configurate"
	"github.com/cyverse-de/echo-middleware/v2/redoc"
	"github.com/cyverse-de/requests/api"
	"github.com/cyverse-de/requests/db"
	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/sirupsen/logrus"
)

var (
	cfgPath = flag.String("config", "/etc/iplant/de/jobservices.yml", "The path to the config file")
	port    = flag.String("port", "8080", "The port to listen to")
	debug   = flag.Bool("debug", false, "Enable debug logging")

	log *logrus.Entry
)

// buildLoggerEntry sets some logging options then returns a logger entry with some custom fields
// for convenience.
func buildLoggerEntry() *logrus.Entry {

	// Enable logging the file name and line number.
	logrus.SetReportCaller(true)

	// Set the logging format to JSON for now because that's what Echo's middleware uses.
	logrus.SetFormatter(&logrus.JSONFormatter{})

	// Enable debugging if we're supposed to.
	if *debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	// Return the custom log entry.
	return logrus.WithFields(logrus.Fields{
		"service": "requests",
		"art-id":  "requests",
		"group":   "org.cyverse",
	})
}
func jaegerTracerProvider(url string) (*tracesdk.TracerProvider, error) {
	// Create the Jaeger exporter
	exp, err := jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(url)))
	if err != nil {
		return nil, err
	}

	tp := tracesdk.NewTracerProvider(
		tracesdk.WithBatcher(exp),
		tracesdk.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("requests"),
		)),
	)

	return tp, nil
}

func init() {
	flag.Parse()

	log = buildLoggerEntry()
	var tracerProvider *tracesdk.TracerProvider

	otelTracesExporter := os.Getenv("OTEL_TRACES_EXPORTER")
	if otelTracesExporter == "jaeger" {
		jaegerEndpoint := os.Getenv("OTEL_EXPORTER_JAEGER_ENDPOINT")
		if jaegerEndpoint == "" {
			log.Warn("Jaeger set as OpenTelemetry trace exporter, but no Jaeger endpoint configured.")
		} else {
			tp, err := jaegerTracerProvider(jaegerEndpoint)
			if err != nil {
				log.Fatal(err)
			}
			tracerProvider = tp
			otel.SetTracerProvider(tp)
			otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
		}
	}

	if tracerProvider != nil {
		tracerCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		defer func(tracerContext context.Context) {
			ctx, cancel := context.WithTimeout(tracerContext, time.Second*5)
			defer cancel()
			if err := tracerProvider.Shutdown(ctx); err != nil {
				log.Fatal(err)
			}
		}(tracerCtx)
	}
}

// CustomValidator represents a validator that Echo can use to check incoming requests.
type CustomValidator struct {
	validator *validator.Validate
}

// Validate performs validation for an incoming request.
func (cv CustomValidator) Validate(i interface{}) error {
	return cv.validator.Struct(i)
}

func main() {
	e := echo.New()

	// Set a custom logger.
	e.Logger = Logger{Entry: log}

	// Register a custom validator.
	e.Validator = &CustomValidator{validator: validator.New()}

	// Add middleware.
	e.Use(otelecho.Middleware("requests"))
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(redoc.Serve(redoc.Opts{Title: "DE Administrative Requests API Documentation"}))

	// Load the service information from the Swagger JSON.
	e.Logger.Info("loading service information")
	serviceInfo, err := getSwaggerServiceInfo()
	if err != nil {
		e.Logger.Fatal(err)
	}

	// Load the configuration file.
	e.Logger.Info("loading the configuration file")
	cfg, err := configurate.Init(*cfgPath)
	if err != nil {
		e.Logger.Fatalf("unable to load the configuration file: %s", err.Error())
	}

	// Initialize the database connection.
	e.Logger.Info("establishing the database connection")
	databaseURI := cfg.GetString("db.uri")
	db, err := db.InitDatabase("postgres", databaseURI)
	if err != nil {
		e.Logger.Fatalf("service initialization failed: %s", err.Error())
	}

	// Create the iplant-email client.
	iplantEmailClient := iplantemail.NewClient(cfg.GetString("iplant_email.base"))

	// Create the iplant-groups client.
	iplantGroupsClient := iplantgroups.NewClient(
		cfg.GetString("iplant_groups.base"),
		cfg.GetString("iplant_groups.user"),
	)

	// Create the notification-agent client.
	notificationAgentClient := notificationagent.NewClient(cfg.GetString("notification_agent.base"))

	// Define the API.
	a := api.API{
		Echo:                    e,
		Title:                   serviceInfo.Title,
		Version:                 serviceInfo.Version,
		DB:                      db,
		UserDomain:              cfg.GetString("users.domain"),
		AdminEmail:              cfg.GetString("email.request"),
		IPlantEmailClient:       iplantEmailClient,
		IPlantGroupsClient:      iplantGroupsClient,
		NotificationAgentClient: notificationAgentClient,
	}

	// Define the API endpoints.
	e.GET("/", a.RootHandler)
	e.GET("/request-types", a.GetRequestTypesHandler)
	e.POST("/request-types/:name", a.RegisterRequestTypeHandler)
	e.PATCH("/request-types/:name", a.UpdateRequestTypesHandler)
	e.GET("/request-types/:name", a.GetRequestTypeHandler)
	e.GET("/request-status-codes", a.GetRequestStatusCodesHandler)
	e.GET("/requests", a.GetRequestsHandler)
	e.POST("/requests", a.AddRequestHandler)
	e.GET("/requests/:id", a.GetRequestDetailsHandler)
	e.POST("/requests/:id/status", a.UpdateRequestHandler)

	// Start the service.
	e.Logger.Info("starting the service")
	e.Logger.Fatal(e.Start(fmt.Sprintf(":%s", *port)))
}
