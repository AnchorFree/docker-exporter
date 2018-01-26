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
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// #include <unistd.h>
//import "C"

const (
	good = iota
	bad

	int64max int64 = int64(9223372036854775807)
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
	dockerContainerStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_container_status",
			Help: "The number of containers found.",
		},
		[]string{"name", "image_id", "docker_container_status"},
	)
	dockerLongestRunning = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_longest_running",
			Help: "Runtime for docker processes",
		},
		[]string{"cmdline"},
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
	prometheus.MustRegister(dockerContainerStatus)
	prometheus.MustRegister(dockerLongestRunning)
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
	imageId := inspect.Image
	log.Printf("Start scraping %s", name)
	perContainerLabels := prometheus.Labels{"name": name, "image_id": imageId}
	timeout := time.Duration(interval.Nanoseconds() * 3 / 4) // 3/4 of the interval seems like a reasonable timeout
	inspectDone := make(chan inspectResult, 1)
	tick := time.Tick(*interval)
	var result inspectResult
	var timeoutContext context.Context
	var cancel context.CancelFunc
	for {
		select {
		case <-closer:
			if cancel != nil {
				cancel()
			}
			closer = nil
			log.Printf("Stop scraping %s", name)
			return
		case <-tick:
			// because we timeout before it is possible to have the next tick,
			// this will not get clobbered by the next tick
			timeoutContext, cancel = context.WithTimeout(context.Background(), timeout)

			go func() {
				inspect, err := cli.ContainerInspect(timeoutContext, container.ID)
				cancel()
				timeoutContext = nil
				cancel = nil
				inspectDone <- inspectResult{inspect, err}
			}()

		case result = <-inspectDone:
			if result.err == nil {
				inspectTimeoutStatus.With(perContainerLabels).Set(good)
			} else {
				if result.err == context.DeadlineExceeded {
					inspectTimeoutStatus.With(perContainerLabels).Set(bad)
					continue
				} else {
					log.Printf("inspecting %s : %s", name, result.err)
				}
			}

			inspect = result.inspect
			restartCounter.With(perContainerLabels).Set(float64(inspect.RestartCount))
			dockerContainerStatus.With(prometheus.Labels{"name": name, "image_id": imageId, "docker_container_status": inspect.State.Status}).Set(1)
			if inspect.State.Health != nil {
				if inspect.State.Health.Status == types.Healthy ||
					inspect.State.Health.Status == types.NoHealthcheck {
					// we treat NoHealthcheck as healthy container, if we got here
					containerHealthStatus.With(perContainerLabels).Set(good)
				} else {
					containerHealthStatus.With(perContainerLabels).Set(bad)
				}
			} else {
				if inspect.State.Running {
					// running container considered as healthy, the rest are not
					containerHealthStatus.With(perContainerLabels).Set(good)
				} else {
					containerHealthStatus.With(perContainerLabels).Set(bad)
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
	pid         int
	ppid        int
	state       rune
	pgrp        int
	sid         int
	tty_nr      int
	tpgit       int
	flags       uint
	minflt      uint32
	cminflt     uint32
	utime       uint32
	stime       uint32
	cutime      int32
	cstime      int32
	nice        int32
	num_threads int32
	itrealvalue int32
	starttime   int64
	vsize       uint32
	rss         int32
	rsslim      uint32
	startcode   uint32
	endcode     uint32
	startstack  uint32
	kstkesp     uint32
	kstkeip     uint32
	signal      uint32
	blocked     uint32

	binary  string
	cmdline string
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
		"%c %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d",
		&p.state,
		&p.ppid,
		&p.pgrp,
		&p.sid,
		&p.tty_nr,
		&p.tpgit,
		&p.flags,
		&p.minflt,
		&p.cminflt,
		&p.utime,
		&p.stime,
		&p.cutime,
		&p.cstime,
		&p.nice,
		&p.num_threads,
		&p.itrealvalue,
		&p.starttime,
		&p.vsize,
		&p.rss,
		&p.rsslim,
		&p.startcode,
		&p.endcode,
		&p.startstack,
		&p.kstkesp,
		&p.kstkeip,
		&p.signal,
		&p.blocked)

	return err
}

func (p *UnixProcess) RefreshCmdline() error {
	cmdlinePath := fmt.Sprintf("/proc/%d/cmdline", p.pid)
	cmdline, err := ioutil.ReadFile(cmdlinePath)
	if err != nil {
		return err
	}
	p.cmdline = string(cmdline)
	return err
}

func newUnixProcess(pid int) (*UnixProcess, error) {
	p := &UnixProcess{pid: pid}
	return p, p.Refresh()
}

type procResult struct {
	zombieCount int
	cmdline     string
	startTime   int64
}

func scrapeProc() (procResult, error) {
	d, err := os.Open("/proc")
	if err != nil {
		return procResult{}, err
	}
	defer d.Close()

	zombieCount := 0
	cmdline := ""
	smallestStartTime := int64max
	for {
		fis, err := d.Readdir(10)
		if err == io.EOF {
			break
		}
		if err != nil {
			return procResult{}, err
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

			// Look for executables with names like [docker-stop] <defunct>
			// I believe the [] in the name is syntactic sugar from ps, or at least this leads to a count of 0 across all servers.
			if p.binary != "docker" {
				continue
			}

			// We are only interested in Zombie processes and per
			// http://man7.org/linux/man-pages/man5/proc.5.html
			// /proc/[pid]/stat state field maps to Z for zombie / defunct
			if p.state != 'Z' {
				zombieCount++
			}

			// Find oldest startcount
			if p.starttime < smallestStartTime {
				p.RefreshCmdline()
				log.Printf("lrp: %s", p.cmdline)
				smallestStartTime = p.starttime
				cmdline = p.cmdline
			}
		}
	}

	return procResult{zombieCount, cmdline, smallestStartTime}, nil
}

type countResult struct {
	scrapeProcResults procResult
	err               error
}

func scrapeDefunct() {
	countDone := make(chan countResult, 1)
	labels := prometheus.Labels{}
	tick := time.Tick(*interval)
	//clockTicksPerSec := float64(C.sysconf(C._SC_CLK_TCK))
	// Linking callouts to C libraries is a nightmare.
	// 100 is the standard value for this, so...
	clockTicksPerSec := float64(100)
	for {
		select {
		case <-tick:
			go func() {
				r, err := scrapeProc()
				countDone <- countResult{r, err}
			}()
		case result := <-countDone:
			if result.err != nil {
				log.Panic(result.err)

			}
			r := result.scrapeProcResults
			zombieProcesses.With(labels).Set(float64(r.zombieCount))
			// From http://man7.org/linux/man-pages/man5/proc.5.html
			// r.startTime is
			// The time the process started after system boot.
			// ... snip ...
			// Since Linux 2.6, the value is expressed
			// in clock ticks (divide by sysconf(_SC_CLK_TCK)).
			s := syscall.Sysinfo_t{}
			err := syscall.Sysinfo(&s)
			if err != nil {
				log.Panic(err)
			}
			c := r.cmdline
			t := float64(s.Uptime) - float64(r.startTime)/clockTicksPerSec
			if t < 0 {
				c = "nil"
				t = 0
			}
			dockerLongestRunning.With(prometheus.Labels{"cmdline": c}).Set(t)
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
