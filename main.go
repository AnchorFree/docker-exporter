package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/context"
)

var (
	// command line arguments
	listen   = flag.String("listen", ":8080", "Address to listen on")
	interval = flag.Duration("interval", 1*time.Minute, "Interval between docker scrapes")

	// prometheus metrics
	restartCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_container_restart_count",
			Help: "Current amount of restarts.",
		},
		[]string{"name", "image_id"},
	)
)

func init() {
	// Metrics have to be registered to be exposed:
	prometheus.MustRegister(restartCount)
}

func main() {
	flag.Parse()

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(*listen, nil))
	}()

	// TODO: Move to dedicated scrape function if we will need to scrape more than one metric
	cli, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	tick := time.Tick(*interval)
	for {
		containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{All: true})
		if err != nil {
			panic(err)
		}

		for _, container := range containers {
			c, _ := cli.ContainerInspect(context.Background(), container.ID)
			restartCount.With(prometheus.Labels{"name": c.Name, "image_id": c.Image}).Set(float64(c.RestartCount))
		}

		<-tick
	}

}
