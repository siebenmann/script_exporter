// Script_exporter is a Prometheus exporter to execute programs and
// scripts and collect metrics from their output and their exit
// status.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ricoberger/script_exporter/pkg/config"
	"github.com/ricoberger/script_exporter/pkg/version"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	namespace                 = "script"
	scriptSuccessHelp         = "# HELP script_success Script exit status (0 = error, 1 = success)."
	scriptSuccessType         = "# TYPE script_success gauge"
	scriptDurationSecondsHelp = "# HELP script_duration_seconds Script execution time, in seconds."
	scriptDurationSecondsType = "# TYPE script_duration_seconds gauge"
)

var (
	exporterConfig config.Config

	listenAddress = flag.String("web.listen-address", ":9469", "Address to listen on for web interface and telemetry.")
	showVersion   = flag.Bool("version", false, "Show version information.")
	createToken   = flag.Bool("create-token", false, "Create bearer token for authentication.")
	configFile    = flag.String("config.file", "config.yaml", "Configuration file in YAML format.")
)

func runScript(args []string) (string, error) {
	var output []byte
	var err error
	output, err = exec.Command(args[0], args[1:]...).Output()
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// instrumentScript wraps the underlying http.Handler with Prometheus
// instrumentation to produce per-script metrics on the number of
// requests in flight, the number of requests in total, and the
// distribution of their duration. Requests without a 'script=' query
// parameter are not instrumented (and will probably be rejected).
func instrumentScript(obs prometheus.ObserverVec, cnt *prometheus.CounterVec, g *prometheus.GaugeVec, next http.Handler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn := r.URL.Query().Get("script")
		if sn == "" {
			// Rather than make up a fake script label, such
			// as "NONE", we let the request fall through without
			// instrumenting it. Under normal circumstances it
			// will fail anyway, as metricsHandler() will
			// reject it.
			next.ServeHTTP(w, r)
			return
		}

		labels := prometheus.Labels{"script": sn}
		g.With(labels).Inc()
		defer g.With(labels).Dec()
		now := time.Now()
		next.ServeHTTP(w, r)
		obs.With(labels).Observe(time.Since(now).Seconds())
		cnt.With(labels).Inc()
	})
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	// Get script from url parameter
	params := r.URL.Query()
	scriptName := params.Get("script")
	if scriptName == "" {
		log.Printf("Script parameter is missing\n")
		http.Error(w, "Script parameter is missing", http.StatusBadRequest)
		return
	}

	// Get prefix from url parameter
	prefix := params.Get("prefix")
	if prefix != "" {
		prefix = fmt.Sprintf("%s_", prefix)
	}

	// Get parameters
	var paramValues []string
	scriptParams := params.Get("params")
	if scriptParams != "" {
		paramValues = strings.Split(scriptParams, ",")

		for i, p := range paramValues {
			paramValues[i] = params.Get(p)
		}
	}

	w.Header().Set("Content-Type", "text/plain")
	scriptStartTime := time.Now()

	// Get and run script
	script := exporterConfig.GetScript(scriptName)
	if script == "" {
		log.Printf("Script not found\n")
		http.Error(w, "Script not found", http.StatusBadRequest)
		return
	}

	output, err := runScript(append(strings.Split(script, " "), paramValues...))
	if err != nil {
		log.Printf("Script failed: %s\n", err.Error())
		fmt.Fprintf(w, "%s\n%s\n%s_success{} %d\n%s\n%s\n%s_duration_seconds{} %f\n", scriptSuccessHelp, scriptSuccessType, namespace, 0, scriptDurationSecondsHelp, scriptDurationSecondsType, namespace, time.Since(scriptStartTime).Seconds())
		return
	}

	// Get ignore output parameter and only return success and duration seconds if 'true'
	outputParam := params.Get("output")
	if outputParam == "ignore" {
		fmt.Fprintf(w, "%s\n%s\n%s_success{} %d\n%s\n%s\n%s_duration_seconds{} %f\n", scriptSuccessHelp, scriptSuccessType, namespace, 1, scriptDurationSecondsHelp, scriptDurationSecondsType, namespace, time.Since(scriptStartTime).Seconds())
		return
	}

	// Format output
	regex1, _ := regexp.Compile("^" + prefix + "\\w*{.*}\\s+")
	regex2, _ := regexp.Compile("^" + prefix + "\\w*{.*}\\s+[0-9|\\.]*")

	var formatedOutput string
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		metric := strings.Trim(scanner.Text(), " ")

		if metric == "" {
			// Do nothing
		} else if metric[0:1] == "#" {
			formatedOutput += fmt.Sprintf("%s\n", metric)
		} else {
			metric = fmt.Sprintf("%s%s", prefix, metric)
			metrics := regex1.FindAllString(metric, -1)
			if len(metrics) == 1 {
				value := strings.Replace(metric[len(metrics[0]):], ",", ".", -1)
				if regex2.MatchString(metrics[0] + value) {
					formatedOutput += fmt.Sprintf("%s%s\n", metrics[0], value)
				}
			}
		}
	}

	fmt.Fprintf(w, "%s\n%s\n%s_success{} %d\n%s\n%s\n%s_duration_seconds{} %f\n%s\n", scriptSuccessHelp, scriptSuccessType, namespace, 1, scriptDurationSecondsHelp, scriptDurationSecondsType, namespace, time.Since(scriptStartTime).Seconds(), formatedOutput)
}

// setupMetrics creates and registers our internal Prometheus metrics,
// and then wraps up a http.HandlerFunc into a http.Handler that
// properly counts all of the metrics when a request happens.
//
// Portions of it are taken from the promhttp examples.
//
// We use the 'scripts' namespace for our internal metrics so that
// they don't collide with the 'script' namespace for probe results.
func setupMetrics(h http.HandlerFunc) http.Handler {
	// Broad metrics provided by promhttp, namespaced into
	// 'http' to make what they're about clear from their
	// names.
	reqs := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "http",
			Name:      "requests_total",
			Help:      "Total requests for scripts by HTTP result code and method.",
		},
		[]string{"code", "method"})
	rdur := prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:  "http",
			Name:       "requests_duration_seconds",
			Help:       "A summary of request durations by HTTP result code and method.",
			Objectives: map[float64]float64{0.25: 0.05, 0.5: 0.05, 0.75: 0.02, 0.9: 0.01, 0.99: 0.001, 1.0: 0.001},
		},
		[]string{"code", "method"})

	// Our per-script metrics, counting requests in flight and
	// requests total, and providing a time distribution.
	sreqs := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "scripts",
			Name:      "requests_total",
			Help:      "Total requests to a script",
		},
		[]string{"script"})
	sif := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "scripts",
			Name:      "requests_inflight",
			Help:      "Number of requests in flight to a script",
		},
		[]string{"script"})
	sdur := prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:  "scripts",
			Name:       "duration_seconds",
			Help:       "A summary of request durations to a script",
			Objectives: map[float64]float64{0.25: 0.05, 0.5: 0.05, 0.75: 0.02, 0.9: 0.01, 0.99: 0.001, 1.0: 0.001},
			//Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
		[]string{"script"},
	)

	// We also publish build information through a metric.
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "scripts",
			Name:      "build_info",
			Help:      "A metric with a constant '1' value labeled by build information.",
		},
		[]string{"version", "revision", "branch", "goversion", "builddate", "builduser"},
	)
	buildInfo.WithLabelValues(version.Version, version.Revision, version.Branch, version.GoVersion, version.BuildDate, version.BuildUser).Set(1)

	prometheus.MustRegister(rdur, reqs, sreqs, sif, sdur, buildInfo)

	// We don't use InstrumentHandlerInFlight, because that
	// duplicates what we're doing on a per-script basis. The
	// other promhttp handlers don't duplicate this work, because
	// they capture result code and method. This is slightly
	// questionable, but there you go.
	return promhttp.InstrumentHandlerDuration(rdur,
		promhttp.InstrumentHandlerCounter(reqs,
			instrumentScript(sdur, sreqs, sif, h)))
}

func main() {
	// Parse command-line flags
	flag.Parse()

	// Show version information
	if *showVersion {
		v, err := version.Print("script_exporter")
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

	// Start exporter
	fmt.Printf("Starting server %s\n", version.Info())
	fmt.Printf("Build context %s\n", version.BuildContext())
	fmt.Printf("script_exporter listening on %s\n", *listenAddress)

	// If authentication is required, it protects the ability to
	// run scripts, which is the most potentially dangerous thing,
	// but not our internal metrics (or the main page HTML). All
	// of our Prometheus metrics about probes are created before
	// any authentication is checked and possibly rejected.
	http.Handle("/probe", setupMetrics(use(metricsHandler, auth)))
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
		<head><title>Script Exporter</title></head>
		<body>
		<h1>Script Exporter</h1>
		<p><a href='/metrics'>Metrics</a></p>
		<p><a href='/probe'>Probe</a></p>
		<p><ul>
		<li>version: ` + version.Version + `</li>
		<li>branch: ` + version.Branch + `</li>
		<li>revision: ` + version.Revision + `</li>
		<li>go version: ` + version.GoVersion + `</li>
		<li>build user: ` + version.BuildUser + `</li>
		<li>build date: ` + version.BuildDate + `</li>
		</ul></p>
		</body>
		</html>`))
	})

	if exporterConfig.TLS.Active {
		log.Fatalln(http.ListenAndServeTLS(*listenAddress, exporterConfig.TLS.Crt, exporterConfig.TLS.Key, nil))
	} else {
		log.Fatalln(http.ListenAndServe(*listenAddress, nil))
	}
}
