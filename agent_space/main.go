package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"agent_space/incident"
	"agent_space/routes"
	"agent_space/utils"
)

// how long in-flight HTTP requests get to finish on shutdown. An /agent run is
// far longer than this, but it is the SQS path that is meant to carry those.
const shutdownGrace = 20 * time.Second

func main() {
	utils.LoadConfig()

	initStore()
	initLLM()
	initMCP()
	defer mcpSess.Close()

	initRepositories()
	// before the worker: the worker only hands verdicts over if this came up
	initRemediation()
	initWorker()

	routes.VectorStore = store
	routes.Model = model
	routes.MCPSession = mcpSess
	routes.SREAgent = sreAgent
	routes.Results = sqsResults
	routes.Remediation = remediator
	routes.Solutions = solutions
	routes.Repos = repos
	routes.Publisher = publisher
	routes.Incidents = incident.NewResolver(pool)

	router := gin.Default()

	// before everything, including /ping: a preflight the frontend sends to a
	// route that does not exist still has to be answered
	router.Use(utils.CORS())

	guarded := router.Group("", utils.RequireToken())

	router.GET("/ping", utils.Ping)
	router.GET("/tools", routes.Tools)

	guarded.POST("/store", routes.Store)
	guarded.POST("/retrieve", routes.Retrieve)
	guarded.POST("/ask", routes.Ask)
	guarded.POST("/agent", routes.Agent)
	guarded.GET("/agent/:id", routes.Investigation)

	// the remediation half, which is what the frontend is built on
	guarded.GET("/repositories", routes.Repositories)
	guarded.GET("/remediations", routes.Remediations)
	guarded.GET("/agent/:id/remediation", routes.RemediationFor)
	guarded.POST("/agent/:id/remediation", routes.StartRemediation)
	guarded.POST("/solutions/:candidate/pr", routes.OpenPullRequest)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// one context for both halves: Ctrl-C stops polling for new assignments and
	// draining HTTP at the same moment
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// the remediation pool outlives any one message: it is started here so its
	// jobs are bounded by the process, not by an SQS visibility timeout
	if remediator != nil {
		remediator.Start(ctx)
	}

	workerDone := make(chan struct{})
	if sqsWorker != nil {
		go func() {
			defer close(workerDone)
			sqsWorker.Run(ctx)
		}()
	} else {
		close(workerDone)
	}

	srv := &http.Server{Addr: ":" + port, Handler: router}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server failed on port %s: %v", port, err)
		}
	}()
	log.Printf("Listening on :%s", port)

	<-ctx.Done()
	log.Println("Shutting down; finishing what is in flight.")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	// an investigation cut short here was never acknowledged, so SQS
	// redelivers it once the visibility timeout lapses
	<-workerDone
}
