package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"

	"launchpad.net/goamz/aws"
	"launchpad.net/goamz/s3"

	"github.com/Luzifer/gobuilder/builddb"
	"github.com/flosch/pongo2"
	"github.com/gorilla/mux"
	"github.com/xuyu/goredis"

	"github.com/Sirupsen/logrus"
	"github.com/Sirupsen/logrus/hooks/papertrail"

	_ "github.com/Luzifer/gobuilder/filters"
	_ "github.com/flosch/pongo2-addons"
)

var s3Bucket *s3.Bucket
var log = logrus.New()
var redisClient *goredis.Redis

func init() {
	log.Out = os.Stderr
	log.Formatter = &logrus.TextFormatter{ForceColors: true}

	papertrail_port, err := strconv.Atoi(os.Getenv("papertrail_port"))
	if err != nil {
		log.Info("Failed to read papertrail_port, using only STDERR")
		return
	}
	hook, err := logrus_papertrail.NewPapertrailHook(os.Getenv("papertrail_host"), papertrail_port, "GoBuilder Frontend")
	if err != nil {
		log.Panic("Unable to create papertrail connection")
		os.Exit(1)
	}

	log.Hooks.Add(hook)

	redisClient, err = goredis.DialURL(os.Getenv("redis_url"))
	if err != nil {
		log.WithFields(logrus.Fields{
			"url": os.Getenv("redis_url"),
		}).Panic("Unable to connect to Redis")
		os.Exit(1)
	}
}

func main() {
	connectS3()

	r := mux.NewRouter()
	registerAPIv1(r)

	r.PathPrefix("/css/").Handler(http.FileServer(http.Dir("./frontend/")))
	r.PathPrefix("/js/").Handler(http.FileServer(http.Dir("./frontend/")))
	r.PathPrefix("/fonts/").Handler(http.FileServer(http.Dir("./frontend/")))
	r.Handle("/favicon.ico", http.FileServer(http.Dir("./frontend/")))

	// Static handlers
	r.HandleFunc("/", handleFrontPage).Methods("GET")
	r.HandleFunc("/contact", handleImprint).Methods("GET")
	r.HandleFunc("/help", handleHelpPage).Methods("GET")

	// Build starters / webhooks (deprecated bv /api/v1/webhook/*)
	r.HandleFunc("/webhook/github", webhookGitHub).Methods("POST")
	r.HandleFunc("/webhook/bitbucket", webhookBitBucket).Methods("POST")

	// Build artifact displaying
	r.HandleFunc("/get/{file:.+}", handlerDeliverFileFromS3).Methods("GET")
	r.HandleFunc("/{repo:.+}/log/{logid}", handlerBuildLog).Methods("GET")
	r.HandleFunc("/{repo:.+}", handlerRepositoryView).Methods("GET")

	http.Handle("/", r)
	http.ListenAndServe(fmt.Sprintf(":%s", os.Getenv("PORT")), nil)
}

func handlerRepositoryView(res http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)
	branch := r.FormValue("branch")
	if branch == "" {
		branch = "master"
	}

	build_status, err := redisClient.Get(fmt.Sprintf("project::%s::build-status", params["repo"]))
	if err != nil || build_status == nil {
		log.WithFields(logrus.Fields{
			"error": fmt.Sprintf("%v", err),
			"repo":  params["repo"],
		}).Warn("AWS S3 Get Error")
		context := getNewBuildContext()
		context["error"] = "Your build is not yet known to us..."
		context["value"] = params["repo"]
		template := pongo2.Must(pongo2.FromFile("frontend/newbuild.html"))
		template.ExecuteWriter(context, res)
		return
	}

	readmeContent, err := s3Bucket.Get(fmt.Sprintf("%s/%s_README.md", params["repo"], branch))
	if err != nil {
		readmeContent = []byte("Project provided no README.md file.")
	}

	buildDurationRaw, err := redisClient.Get(fmt.Sprintf("project::%s::build-duration", params["repo"]))
	if err != nil || len(buildDurationRaw) == 0 {
		buildDurationRaw = []byte("0")
	}
	buildDuration, err := strconv.Atoi(string(buildDurationRaw))
	if err != nil {
		buildDuration = 0
	}

	signature, err := redisClient.Get(fmt.Sprintf("project::%s::signatures::%s", params["repo"], branch))
	if err != nil {
		signature = []byte("")
	}

	buildDB := builddb.BuildDB{}
	hasBuilds := false

	file, err := getBuildDBWithFallback(params["repo"])
	if err != nil {
		buildDB["master"] = builddb.BuildDBBranch{}
		hasBuilds = false
	} else {
		err = json.Unmarshal(file, &buildDB)
		if err != nil {
			log.WithFields(logrus.Fields{
				"error": fmt.Sprintf("%v", err),
			}).Error("AWS DB Unmarshal Error")
			context := getNewBuildContext()
			context["error"] = "An unknown error occured while getting your build."
			template := pongo2.Must(pongo2.FromFile("frontend/newbuild.html"))
			template.ExecuteWriter(context, res)
			return
		}
		hasBuilds = true
	}

	logs, err := redisClient.ZRevRange(fmt.Sprintf("project::%s::logs", params["repo"]), 0, 10, false)
	if err != nil {
		logs = []string{}
		log.WithFields(logrus.Fields{
			"repo": params["repo"],
			"err":  err,
		}).Error("Unable to load last logs")
	}

	template := pongo2.Must(pongo2.FromFile("frontend/repository.html"))
	branches := []builddb.BranchSortEntry{}
	for k, v := range buildDB {
		branches = append(branches, builddb.BranchSortEntry{Branch: k, BuildDate: v.BuildDate})
	}
	sort.Sort(sort.Reverse(builddb.BranchSortEntryByBuildDate(branches)))
	template.ExecuteWriter(pongo2.Context{
		"branch":        branch,
		"branches":      branches,
		"repo":          params["repo"],
		"mybranch":      buildDB[branch],
		"build_status":  string(build_status),
		"readme":        string(readmeContent),
		"hasbuilds":     hasBuilds,
		"buildDuration": buildDuration,
		"signature":     string(signature),
		"logs":          logs,
	}, res)
}

func connectS3() {
	s3auth, err := aws.EnvAuth()
	if err != nil {
		panic(err)
	}

	s3conn := s3.New(s3auth, aws.Regions["eu-west-1"])
	bucket := s3conn.Bucket("gobuild.luzifer.io")

	s3Bucket = bucket
}

func getBuildDBWithFallback(repo string) ([]byte, error) {
	redisKey := fmt.Sprintf("project::%s::builddb", repo)
	buildDB, err := redisClient.Get(redisKey)
	if err != nil || len(buildDB) == 0 {
		// Fall back to old storage method
		buildDB, err = s3Bucket.Get(fmt.Sprintf("%s/build.db", repo))
		if err != nil {
			log.WithFields(logrus.Fields{
				"error": err,
				"repo":  repo,
			}).Error("Failed to load build.db")
			return []byte{}, fmt.Errorf("Unable to load build.db: %s", err)
		}
	}

	return buildDB, nil
}
