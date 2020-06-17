package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"plugin"
	"runtime"
	"strconv"
	"strings"

	"github.com/appbaseio/arc/middleware"
	"github.com/appbaseio/arc/middleware/logger"
	"github.com/appbaseio/arc/plugins"
	"github.com/appbaseio/arc/util"
	"github.com/gorilla/mux"
	"github.com/robfig/cron"
	"github.com/rs/cors"

	log "github.com/sirupsen/logrus"
)

const logTag = "[cmd]"

var (
	envFile     string
	logMode     string
	listPlugins bool
	address     string
	port        int
	pluginDir   string
	https       bool
	// Version arc version set during build
	Version string
	// PlanRefreshInterval can be used to define the custom interval to refresh the plan
	PlanRefreshInterval string
	// Billing is a build time flag
	Billing string
	// HostedBilling is a build time flag
	HostedBilling string
	// ClusterBilling is a build time flag
	ClusterBilling string
	// IgnoreBillingMiddleware ignores the billing middleware
	IgnoreBillingMiddleware string

	// Tier for testing
	Tier string
	// FeatureCustomEvents for testing
	FeatureCustomEvents string
	// FeatureSuggestions for testing
	FeatureSuggestions string
	// FeatureRules for testing
	FeatureRules string
	// FeatureTemplates for testing
	FeatureTemplates string
	// FeatureFunctions for testing
	FeatureFunctions string
	// FeatureSearchRelevancy for testing
	FeatureSearchRelevancy string
)

func init() {
	flag.StringVar(&envFile, "env", ".env", "Path to file with environment variables to load in KEY=VALUE format")
	flag.StringVar(&logMode, "log", "", "Define to change the default log mode(error), other options are: debug(most verbose) and info")
	flag.BoolVar(&listPlugins, "plugins", false, "List currently registered plugins")
	flag.StringVar(&address, "addr", "", "Address to serve on")
	flag.IntVar(&port, "port", 8000, "Port number")
	flag.StringVar(&pluginDir, "pluginDir", "build/plugins", "Directory containing the compiled plugins")
	flag.BoolVar(&https, "https", false, "Starts a https server instead of a http server if true")
}

func main() {
	flag.Parse()
	log.SetReportCaller(true)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:          true,
		TimestampFormat:        "2006/01/02 15:04:05",
		DisableLevelTruncation: true,
		CallerPrettyfier: func(f *runtime.Frame) (string, string) {
			filename := path.Base(f.File)
			return "", fmt.Sprintf(" %s:%d", filename, f.Line)
		},
	})

	switch logMode {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	default:
		log.SetLevel(log.ErrorLevel)
	}

	// Load all env vars from envFile
	if err := LoadEnvFromFile(envFile); err != nil {
		log.Errorln(logTag, ": reading env file", envFile, ": ", err)
	}

	router := mux.NewRouter().StrictSlash(true)

	if PlanRefreshInterval == "" {
		PlanRefreshInterval = "1"
	} else {
		_, err := strconv.Atoi(PlanRefreshInterval)
		if err != nil {
			log.Fatal("PLAN_REFRESH_INTERVAL must be an integer: ", err)
		}
	}

	interval := "@every " + PlanRefreshInterval + "h"

	util.Billing = Billing
	util.HostedBilling = HostedBilling
	util.ClusterBilling = ClusterBilling
	util.Version = Version

	if Billing == "true" {
		log.Println("You're running Arc with billing module enabled.")
		util.ReportUsage()
		cronjob := cron.New()
		cronjob.AddFunc(interval, util.ReportUsage)
		cronjob.Start()
		if IgnoreBillingMiddleware != "true" {
			router.Use(util.BillingMiddleware)
		}
	} else if HostedBilling == "true" {
		log.Println("You're running Arc with hosted billing module enabled.")
		util.ReportHostedArcUsage()
		cronjob := cron.New()
		cronjob.AddFunc(interval, util.ReportHostedArcUsage)
		cronjob.Start()
		if IgnoreBillingMiddleware != "true" {
			router.Use(util.BillingMiddleware)
		}
	} else if ClusterBilling == "true" {
		log.Println("You're running Arc with cluster billing module enabled.")
		util.SetClusterPlan()
		// refresh plan
		cronjob := cron.New()
		cronjob.AddFunc(interval, util.SetClusterPlan)
		cronjob.Start()
		if IgnoreBillingMiddleware != "true" {
			router.Use(util.BillingMiddleware)
		}
	} else {
		util.SetDefaultTier()
		log.Println("You're running Arc with billing module disabled.")
	}

	// Testing Env: Set variables based on the build blags
	if Tier != "" {
		var temp1 = map[string]interface{}{
			"tier": Tier,
		}
		type Temp struct {
			Tier *util.Plan `json:"tier"`
		}
		temp2 := Temp{}
		mashalled, err := json.Marshal(temp1)
		if err != nil {
			log.Fatal(err)
		}
		err = json.Unmarshal(mashalled, &temp2)
		if err != nil {
			log.Fatal(err)
		}
		util.SetTier(temp2.Tier)
	}
	if FeatureCustomEvents != "" && FeatureCustomEvents == "true" {
		util.SetFeatureCustomEvents(true)
	}
	if FeatureSuggestions != "" && FeatureSuggestions == "true" {
		util.SetFeatureSuggestions(true)
	}
	if FeatureRules != "" && FeatureRules == "true" {
		util.SetFeatureRules(true)
	}
	if FeatureFunctions != "" && FeatureFunctions == "true" {
		util.SetFeatureFunctions(true)
	}
	if FeatureTemplates != "" && FeatureTemplates == "true" {
		util.SetFeatureTemplates(true)
	}
	if FeatureSearchRelevancy != "" && FeatureSearchRelevancy == "true" {
		util.SetFeatureSearchRelevancy(true)
	}

	// ES client instantiation
	// ES v7 and v6 clients
	util.NewClient()
	util.SetDefaultIndexTemplate()
	// map of specific plugins
	sequencedPlugins := []string{"searchrelevancy.so", "rules.so", "functions.so", "analytics.so", "suggestions.so"}
	sequencedPluginsByPath := make(map[string]string)

	var elasticSearchPath, reactiveSearchPath string
	elasticSearchMiddleware := make([]middleware.Middleware, 0)
	reactiveSearchMiddleware := make([]middleware.Middleware, 0)
	err := filepath.Walk(pluginDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(info.Name()) == ".so" && info.Name() != "elasticsearch.so" {
			if info.Name() != "querytranslate.so" {
				if util.IsExists(info.Name(), sequencedPlugins) {
					sequencedPluginsByPath[info.Name()] = path
				} else {
					plugin, err1 := LoadPluginFromFile(router, path)
					if err1 != nil {
						return err1
					}
					reactiveSearchMiddleware = append(reactiveSearchMiddleware, plugin.RSMiddleware()...)
					elasticSearchMiddleware = append(elasticSearchMiddleware, plugin.ESMiddleware()...)
				}
			} else {
				reactiveSearchPath = path
			}
		} else if info.Name() == "elasticsearch.so" {
			elasticSearchPath = path
		}
		return nil
	})
	// load plugins in a sequence
	for _, pluginName := range sequencedPlugins {
		path, _ := sequencedPluginsByPath[pluginName]
		if path != "" {
			plugin, err := LoadPluginFromFile(router, path)
			if err != nil {
				log.Fatal("error loading plugins: ", err)
			}
			elasticSearchMiddleware = append(elasticSearchMiddleware, plugin.ESMiddleware()...)
			reactiveSearchMiddleware = append(reactiveSearchMiddleware, plugin.RSMiddleware()...)
		}
	}
	// Load ReactiveSearch plugin
	if reactiveSearchPath != "" {
		LoadRSPluginFromFile(router, reactiveSearchPath, reactiveSearchMiddleware)
	}
	LoadESPluginFromFile(router, elasticSearchPath, elasticSearchMiddleware)
	if err != nil {
		log.Fatal("error loading plugins: ", err)
	}

	// CORS policy
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"HEAD", "GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"*"},
		ExposedHeaders: []string{"*"},
	})
	handler := c.Handler(router)
	handler = logger.Log(handler)

	// Listen and serve ...
	addr := fmt.Sprintf("%s:%d", address, port)
	log.Println(logTag, ":listening on", addr)
	if https {
		httpsCert := os.Getenv("HTTPS_CERT")
		httpsKey := os.Getenv("HTTPS_KEY")
		log.Fatal(http.ListenAndServeTLS(addr, httpsCert, httpsKey, handler))
	} else {
		log.Fatal(http.ListenAndServe(addr, handler))
	}
}

func LoadPIFromFile(path string) (plugin.Symbol, error) {
	pf, err1 := plugin.Open(path)
	if err1 != nil {
		return nil, err1
	}
	return pf.Lookup("PluginInstance")
}

// LoadPluginFromFile loads a plugin at the given location
func LoadPluginFromFile(router *mux.Router, path string) (plugins.Plugin, error) {
	pi, err2 := LoadPIFromFile(path)
	if err2 != nil {
		return nil, err2
	}
	var p plugins.Plugin
	p = *pi.(*plugins.Plugin)
	err3 := plugins.LoadPlugin(router, p)
	if err3 != nil {
		return nil, err3
	}
	return p, nil
}

func LoadESPluginFromFile(router *mux.Router, path string, mw []middleware.Middleware) error {
	pi, err2 := LoadPIFromFile(path)
	if err2 != nil {
		return err2
	}
	var p plugins.ESPlugin
	p = *pi.(*plugins.ESPlugin)
	return plugins.LoadESPlugin(router, p, mw)
}

func LoadRSPluginFromFile(router *mux.Router, path string, mw []middleware.Middleware) error {
	pi, err2 := LoadPIFromFile(path)
	if err2 != nil {
		return err2
	}
	var p plugins.RSPlugin
	p = *pi.(*plugins.RSPlugin)
	return plugins.LoadRSPlugin(router, p, mw)
}

// LoadEnvFromFile loads env vars from envFile. Envs in the file
// should be in KEY=VALUE format.
func LoadEnvFromFile(envFile string) error {
	if envFile == "" {
		return nil
	}

	file, err := os.Open(envFile)
	if err != nil {
		return err
	}
	defer file.Close()

	envMap, err := ParseEnvFile(file)
	if err != nil {
		return err
	}

	for k, v := range envMap {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}

	return nil
}

// ParseEnvFile parses the envFile for env variables in present in
// KEY=VALUE format. It ignores the comment lines starting with "#".
func ParseEnvFile(envFile io.Reader) (map[string]string, error) {
	envMap := make(map[string]string)

	scanner := bufio.NewScanner(envFile)
	var line string
	lineNumber := 0

	for scanner.Scan() {
		line = strings.TrimSpace(scanner.Text())
		lineNumber++

		// skip the lines starting with comment
		if strings.HasPrefix(line, "#") {
			continue
		}

		// skip empty line
		if len(line) == 0 {
			continue
		}

		fields := strings.SplitN(line, "=", 2)
		if len(fields) != 2 {
			return nil, fmt.Errorf("can't parse line %d; line should be in KEY=VALUE format", lineNumber)
		}

		// KEY should not contain any whitespaces
		if strings.Contains(fields[0], " ") {
			return nil, fmt.Errorf("can't parse line %d; KEY contains whitespace", lineNumber)
		}

		key := fields[0]
		value := fields[1]

		if key == "" {
			return nil, fmt.Errorf("can't parse line %d; KEY can't be empty string", lineNumber)
		}
		envMap[key] = value
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return envMap, nil
}
