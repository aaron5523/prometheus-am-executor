package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/aaron5523/prometheus-am-executor/pkg/config"
	_ "github.com/aaron5523/prometheus-am-executor/pkg/version"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

var (
	listenAddr      = flag.String("l", ":8080", "HTTP Port to listen on")
	verbose         = flag.Bool("v", false, "Enable verbose/debug logging")

	exporterConfig config.Config

	showVersion   = flag.Bool("version", false, "Show version information.")
	createToken   = flag.Bool("create-token", false, "Create bearer token for authentication.")
	configFile    = flag.String("config.file", "config.yaml", "Configuration `file` in YAML format.")

	timeoutOffset = flag.Float64("timeout-offset", 0.5, "Offset to subtract from Prometheus-supplied timeout in `seconds`.")

	processDuration = prometheus.NewHistogram(prometheus.HistogramOpts{

		Namespace: "am_executor",
		Subsystem: "process",
		Name:      "duration_seconds",
		Help:      "Time the processes handling alerts ran.",
		Buckets:   []float64{1, 10, 60, 600, 900, 1800},
	})

	processesCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "am_executor",
		Subsystem: "processes",
		Name:      "current",
		Help:      "Current number of processes running.",
	})

	errCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "am_executor",
		Subsystem: "errors",
		Name:      "total",
		Help:      "Total number of errors while processing alerts.",
	}, []string{"stage"})

	rnr *runner
)

func handleError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
	log.Println(err)
}

func handleWebhook(w http.ResponseWriter, req *http.Request) {
	if *verbose {
		log.Println("Webhook triggered")
	}
	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		handleError(w, err)
		errCounter.WithLabelValues("read")
		return
	}
	if *verbose {
		log.Println("Body:", string(data))
	}
	payload := &template.Data{}
	if err := json.Unmarshal(data, payload); err != nil {
		handleError(w, err)
		errCounter.WithLabelValues("unmarshal")
	}
	if *verbose {
		log.Printf("Got: %#v", payload)
	}
	if err := rnr.run(amDataToEnv(payload)); err != nil {
		handleError(w, err)
		errCounter.WithLabelValues("start")
	}
}

func handleHealth(w http.ResponseWriter, req *http.Request) {
	fmt.Fprint(w, "All systems are functioning within normal specifications.\n")
}

type logWriter struct{}

func (lw *logWriter) Write(p []byte) (n int, err error) {
	log.Print(string(p))
	return len(p), nil
}

type runner struct {
	command   string
	args      []string
	processes []exec.Cmd
}

func (r *runner) run(env []string) error {
	lw := &logWriter{}
	processesCurrent.Inc()
	defer processesCurrent.Dec()
	cmd := exec.Command(r.command, r.args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = lw
	cmd.Stderr = lw
	return cmd.Run()
}

func timeToStr(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.Itoa(int(t.Unix()))
}

func amDataToEnv(td *template.Data) []string {
	env := []string{
		"AMX_RECEIVER=" + td.Receiver,
		"AMX_STATUS=" + td.Status,
		"AMX_EXTERNAL_URL=" + td.ExternalURL,
		"AMX_ALERT_LEN=" + strconv.Itoa(len(td.Alerts)),
	}
	for p, m := range map[string]map[string]string{
		"AMX_LABEL":      td.CommonLabels,
		"AMX_GLABEL":     td.GroupLabels,
		"AMX_ANNOTATION": td.CommonAnnotations,
	} {
		for k, v := range m {
			env = append(env, p+"_"+k+"="+v)
		}
	}

	for i, alert := range td.Alerts {
		key := "AMX_ALERT_" + strconv.Itoa(i+1)
		env = append(env,
			key+"_STATUS"+"="+alert.Status,
			key+"_START"+"="+timeToStr(alert.StartsAt),
			key+"_END"+"="+timeToStr(alert.EndsAt),
			key+"_URL"+"="+alert.GeneratorURL,
		)
		for p, m := range map[string]map[string]string{
			"LABEL":      alert.Labels,
			"ANNOTATION": alert.Annotations,
		} {
			for k, v := range m {
				env = append(env, key+"_"+p+"_"+k+"="+v)
			}
		}
	}
	return env
}

func main() {

	prometheus.MustRegister(processDuration)
	prometheus.MustRegister(processesCurrent)
	prometheus.MustRegister(errCounter)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] script [args..]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// Show version information
	if *showVersion {
		v, err := version.Print("prometheus-am-executor")
		if err != nil {
			log.Fatalf("Failed to print version information: %#v", err)
		}

		fmt.Fprintln(os.Stdout, v)
		os.Exit(0)
	}

	// Load configuration file
	err := exporterConfig.LoadConfig(*configFile)
	if err != nil {
		log.Fatalln(err)
	}

	// Create bearer token
	if *createToken {
		token, err := createJWT()
		if err != nil {
			log.Fatalf("Bearer token could not be created: %s\n", err.Error())
		}

		fmt.Printf("Bearer token: %s\n", token)
		os.Exit(0)
	}


	command := flag.Args()
	if len(command) == 0 {
		log.Fatal("Require command")
	}
	rnr = &runner{
		command: command[0],
	}
	if len(command) > 1 {
		rnr.args = command[1:]
	}
	//http.HandleFunc("/", handleWebhook)
	//http.HandleFunc("/_health", handleHealth)
 	//http.Handle("/metrics", promhttp.Handler())
	//log.Println("Listening on", *listenAddr, "and running", command)
	//log.Fatal(http.ListenAndServe(*listenAddr, nil))
	// Start exporter
	fmt.Printf("Starting server %s\n", version.Info())
	fmt.Printf("Build context %s\n", version.BuildContext())
	fmt.Printf("script_exporter listening on %s\n", *listenAddr)

	// Authentication can be enabled via the 'basicAuth' or 'bearerAuth'
	// section in the configuration. If authentication is enabled it's
	// required for all routes.
	router := http.NewServeMux()

	//router.Handle("/probe", setupMetrics(metricsHandler))
	router.Handle("/metrics", promhttp.Handler())
	router.HandleFunc("/", handleWebhook)

	server := &http.Server{
		Addr:    *listenAddr,
		Handler: auth(router),
	}

	// Listen for SIGINT and SIGTERM signals and try to gracefully shutdown
	// the HTTP server. This ensures that enabled connections are not
	// interrupted.
	go func() {
		term := make(chan os.Signal, 1)
		signal.Notify(term, os.Interrupt, syscall.SIGTERM)
		select {
		case <-term:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := server.Shutdown(ctx)
			if err != nil {
				log.Printf("Failed to shutdown script_exporter gracefully: %s\n", err.Error())
				os.Exit(1)
			}

			log.Printf("Shutdown script_exporter...\n")
			os.Exit(0)
		}
	}()

	// Listen for SIGHUP signal and reload the configuration. If the
	// configuration could not be reloaded, the old config will continue to be
	// used.
	go func() {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		select {
		case <-hup:
			err := exporterConfig.LoadConfig(*configFile)
			if err != nil {
				log.Printf("Could not reload configuration: %s\n", err.Error())
			} else {
				log.Printf("Configuration reloaded\n")
			}
		}
	}()

	if exporterConfig.TLS.Enabled {
		log.Fatalln(server.ListenAndServeTLS(exporterConfig.TLS.Crt, exporterConfig.TLS.Key))
	} else {
		log.Fatalln(server.ListenAndServe())
	}
}
