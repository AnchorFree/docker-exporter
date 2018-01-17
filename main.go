package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	good = iota
	bad
)

var (
	wg sync.WaitGroup

	// example: 17.09.0-ce
	versionScrubber = regexp.MustCompile(`[^\d]+`)

	// command line arguments
	listen   = flag.String("listen", ":8080", "Address to listen on")
	interval = flag.Duration("interval", 1*time.Minute, "Interval between docker scrapes")

	// maps container id -> closer channel
	scrapers = make(map[string]chan bool)

	// prometheus metrics
	restartCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_container_restart_count",
			Help: "Current amount of restarts.",
		},
		[]string{"name", "image_id"},
	)
	containerHealthStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_container_healthy",
			Help: "Healthy and running if 0, and 1 if anything else",
		},
		[]string{"name", "image_id"},
	)
	inspectTimeoutStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_container_stuck_inspect",
			Help: "Inspect worked if 0, and 1 if timed out",
		},
		[]string{"name", "image_id"},
	)
	dockerVersion = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_version",
			Help: "The version of dockerd",
		},
		[]string{"docker_version"},
	)
	zombieProcesses = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_zombie_processes",
			Help: "Current number of zombie processes that start with [docker",
		},
		[]string{},
	)
	dockerContainerCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_container_count",
			Help: "The number of containers found.",
		},
		[]string{"docker_container_count"},
	)
)

func init() {
	// Metrics have to be registered to be exposed:
	prometheus.MustRegister(restartCounter)
	prometheus.MustRegister(containerHealthStatus)
	prometheus.MustRegister(inspectTimeoutStatus)
	prometheus.MustRegister(dockerVersion)
	prometheus.MustRegister(zombieProcesses)
	prometheus.MustRegister(dockerContainerCount)
}

type inspectResult struct {
	inspect types.ContainerJSON
	err     error
}

func scrapeContainer(container types.Container, cli *client.Client, closer <-chan bool) {
	inspect, err := cli.ContainerInspect(context.Background(), container.ID)
	if err != nil {
		log.Printf("first inspect failed for container %s : %s", container.ID, err)
		return
	}
	name := inspect.Name
	log.Printf("Start scraping %s", name)
	labels := prometheus.Labels{"name": name, "image_id": inspect.Image}
	timeout := time.Duration(interval.Nanoseconds() * 3 / 4) // 3/4 of the interval seems like a reasonable timeout
	inspectDone := make(chan inspectResult, 1)
	tick := time.Tick(*interval)
	var result inspectResult
	for {
		select {
		case <-closer:
			closer = nil
			log.Printf("Stop scraping %s", name)
			return
		case <-tick:
			timeoutContext, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			go func() {
				inspect, err := cli.ContainerInspect(timeoutContext, container.ID)
				inspectDone <- inspectResult{inspect, err}
			}()

		case result = <-inspectDone:
			if result.err == nil {
				inspectTimeoutStatus.With(labels).Set(good)
			} else {
				if result.err == context.DeadlineExceeded {
					inspectTimeoutStatus.With(labels).Set(bad)
					continue
				} else {
					log.Printf("inspecting %s : %s", name, result.err)
				}
			}

			inspect = result.inspect
			restartCounter.With(labels).Set(float64(inspect.RestartCount))
			if result.inspect.State.Health != nil {
				if result.inspect.State.Health.Status == types.Healthy ||
					result.inspect.State.Health.Status == types.NoHealthcheck {
					// we treat NoHealthcheck as healthy container, if we got here
					containerHealthStatus.With(labels).Set(good)
				} else {
					containerHealthStatus.With(labels).Set(bad)
				}
			} else {
				if result.inspect.State.Running {
					// running container considered as healthy, the rest are not
					containerHealthStatus.With(labels).Set(good)
				} else {
					containerHealthStatus.With(labels).Set(bad)
				}
			}
		}
	}
}

func scrapeContainers(cli *client.Client) {
	tick := time.Tick(*interval)
	for {
		// Since we will never close scrapeContainers, don't bother to make this async following the pattern in scrapeContainer
		containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{All: true})
		if err != nil {
			log.Panic(err)
		}

		newScrapers := make(map[string]chan bool)
		containerCount := 0
		for _, container := range containers {
			containerCount++
			if _, present := scrapers[container.ID]; present {
				newScrapers[container.ID] = scrapers[container.ID]
			} else {
				//log.Printf("new container: %s", container.ID)
				newScrapers[container.ID] = make(chan bool)
				go scrapeContainer(container, cli, newScrapers[container.ID])
			}
		}
		dockerContainerCount.With(prometheus.Labels{"docker_container_count": strconv.Itoa(containerCount)}).Set(float64(containerCount))

		// detect containers which have gone away and kill their scrapers
		for containerId, closer := range scrapers {
			if _, present := newScrapers[containerId]; !present {
				//log.Printf("container gone: %s", containerId)
				closer <- true
				close(closer)
			}
		}

		scrapers = newScrapers

		<-tick
	}
}

func scrapeDockerVersion(cli *client.Client) {
	tick := time.Tick(*interval)
	//log.Printf("Starting docker version scraper")

	for {
		// Since we will never close scrapeContainers, don't bother to make this async following the pattern in scrapeContainer
		server, err := cli.ServerVersion(context.Background())
		if err != nil {
			log.Panic(err)
		}
		//log.Printf("version: %s", server.Version)
		scrubbedVersion := versionScrubber.ReplaceAllString(server.Version, "")
		dockerVersionFloat, err := strconv.ParseFloat(scrubbedVersion, 64)
		if err != nil {
			log.Panic(err)
		}
		dockerVersion.With(prometheus.Labels{"docker_version": server.Version}).Set(dockerVersionFloat)

		<-tick
	}
}

type UnixProcess struct {
	pid   int
	ppid  int
	state rune
	pgrp  int
	sid   int

	binary string
}

func (p *UnixProcess) Refresh() error {
	statPath := fmt.Sprintf("/proc/%d/stat", p.pid)
	dataBytes, err := ioutil.ReadFile(statPath)
	if err != nil {
		return err
	}

	// First, parse out the image name
	// Example stat string:
	// 9249 (haproxy) S 5003 5003 5003 0 -1 4194560 21133 0 0 0 36 2 0 0 20 0 1 0 176991 99676160 21269 18446744073709551615 94539338215424 94539339626788 140727663170416 140727663169928 140237674944734 0 0 4096 3146245 1 0 0 17 4 0 0 0 0 0 94539341725792 94539341790336 94539349725184 140727663173220 140727663173296 140727663173296 140727663173600 0
	data := string(dataBytes)
	binStart := strings.IndexRune(data, '(') + 1
	binEnd := strings.IndexRune(data[binStart:], ')')
	p.binary = data[binStart : binStart+binEnd]

	// Move past the image name and start parsing the rest
	data = data[binStart+binEnd+2:]
	_, err = fmt.Sscanf(data,
		"%c %d %d %d",
		&p.state,
		&p.ppid,
		&p.pgrp,
		&p.sid)

	return err
}

func newUnixProcess(pid int) (*UnixProcess, error) {
	p := &UnixProcess{pid: pid}
	return p, p.Refresh()
}

func zombieDockerProcessCount() (int, error) {
	d, err := os.Open("/proc")
	if err != nil {
		return -1, err
	}
	defer d.Close()

	zombieCount := 0
	for {
		fis, err := d.Readdir(10)
		if err == io.EOF {
			break
		}
		if err != nil {
			return -1, err
		}

		for _, fi := range fis {
			// We only care about directories, since all pids are dirs
			if !fi.IsDir() {
				continue
			}

			// We only care if the name starts with a numeric
			name := fi.Name()
			if name[0] < '0' || name[0] > '9' {
				continue
			}

			// From this point forward, any errors we just ignore, because
			// it might simply be that the process doesn't exist anymore.
			pid, err := strconv.ParseInt(name, 10, 0)
			if err != nil {
				continue
			}

			p, err := newUnixProcess(int(pid))
			if err != nil {
				continue
			}

			// We are only interested in Zombie processes and per
			// http://man7.org/linux/man-pages/man5/proc.5.html
			// /proc/[pid]/stat state field maps to Z for zombie / defunct
			if p.state != 'Z' {
				continue
			}

			// Look for executables with names like [docker-stop] <defunct>
			if strings.HasPrefix(p.binary, "[docker") {
				zombieCount++
			}
		}
	}

	return zombieCount, nil
}

type countResult struct {
	count int
	err   error
}

func scrapeDefunct() {
	var countDone chan countResult // See https://talks.golang.org/2013/advconc.slide#39
	labels := prometheus.Labels{}
	tick := time.Tick(*interval)
	for {
		select {
		case <-tick:
			countDone = make(chan countResult, 1)
			go func() {
				zombieCount, err := zombieDockerProcessCount()
				countDone <- countResult{zombieCount, err}
			}()
		case result := <-countDone:
			countDone = nil
			if result.err != nil {
				log.Panic(result.err)

			}
			zombieProcesses.With(labels).Set(float64(result.count))
		}
	}
}

func main() {
	flag.Parse()
	wg.Add(1)

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(*listen, nil))
	}()

	cli, err := client.NewEnvClient()
	if err != nil {
		log.Panic(err)
	}

	go scrapeDockerVersion(cli)

	go scrapeContainers(cli)

	go scrapeDefunct()

	wg.Wait() // will never end
}
