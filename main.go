package main

import (
	"flag"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	log "github.com/Sirupsen/logrus"
	"github.com/miekg/dns"
)

var (
	debug       = flag.Bool("debug", false, "Debug")
	listen      = flag.String("listen", ":53", "Address to listen to (TCP and UDP)")
	answersFile = flag.String("answers", "./answers.json", "File containing the answers to respond with")
	ttl         = flag.Uint("ttl", 600, "TTL for answers")
	logFile     = flag.String("log", "", "Log file")
	pidFile     = flag.String("pid-file", "", "PID to write to")

	answers Answers
)

func main() {
	log.Info("Starting rancher-dns")
	parseFlags()
	loadAnswers()
	watchSignals()

	udpServer := &dns.Server{Addr: *listen, Net: "udp"}
	tcpServer := &dns.Server{Addr: *listen, Net: "tcp"}

	dns.HandleFunc(".", route)

	go func() {
		log.Fatal(udpServer.ListenAndServe())
	}()
	log.Info("Listening on ", *listen)
	log.Fatal(tcpServer.ListenAndServe())
}

func parseFlags() {
	flag.Parse()

	if *debug {
		log.SetLevel(log.DebugLevel)
	}

	if *logFile != "" {
		if output, err := os.OpenFile(*logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666); err != nil {
			log.Fatalf("Failed to log to file %s: %v", *logFile, err)
		} else {
			log.SetOutput(output)
		}
	}

	if *pidFile != "" {
		log.Infof("Writing pid %d to %s", os.Getpid(), *pidFile)
		if err := ioutil.WriteFile(*pidFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
			log.Fatalf("Failed to write pid file %s: %v", *pidFile, err)
		}
	}
}

func loadAnswers() {
	if temp, err := ReadAnswersFile(*answersFile); err == nil {
		answers = temp
		log.Info("Loaded answers for ", len(answers), " IPs")
	} else {
		log.Errorf("Failed to reload answers: %v", err)
	}
}

func watchSignals() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)

	go func() {
		for _ = range c {
			log.Info("Received HUP signal, reloading answers")
			loadAnswers()
		}
	}()
}

func route(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) == 0 {
		dns.HandleFailed(w, req)
		return
	}

	clientIp, _, _ := net.SplitHostPort(w.RemoteAddr().String())
	question := req.Question[0]
	// We are assuming the JSON config has all names as lower case
	fqdn := strings.ToLower(question.Name)
	rrType := dns.Type(req.Question[0].Qtype).String()

	log.WithFields(log.Fields{
		"question": question.Name,
		"type":     rrType,
		"client":   clientIp,
	}).Debug("Request")

	// Client-specific answers
	found, ok := answers.LocalAnswer(fqdn, rrType, clientIp)
	if ok {
		log.WithFields(log.Fields{
			"client":   clientIp,
			"type":     rrType,
			"question": question.Name,
			"source":   "client",
			"found":    len(found),
		}).Info("Found match for client")

		Respond(w, req, found)
		return
	} else {
		log.Debug("No match found for client")
	}

	// Not-client-specific answers
	found, ok = answers.DefaultAnswer(fqdn, rrType, clientIp)
	if ok {
		log.WithFields(log.Fields{
			"client":   clientIp,
			"type":     rrType,
			"question": question.Name,
			"source":   "default",
			"found":    len(found),
		}).Info("Found match in ", DEFAULT_KEY)

		Respond(w, req, found)
		return
	} else {
		log.Debug("No match found in ", DEFAULT_KEY)
	}

	// Phone a friend
	var recurseHosts Zone
	found, ok = answers.Matching(clientIp, RECURSE_KEY)
	if ok {
		recurseHosts = append(recurseHosts, found...)
	}
	found, ok = answers.Matching(DEFAULT_KEY, RECURSE_KEY)
	if ok {
		recurseHosts = append(recurseHosts, found...)
	}

	var err error
	for _, addr := range recurseHosts {
		err = Proxy(w, req, addr)
		if err == nil {
			log.WithFields(log.Fields{
				"client":   clientIp,
				"type":     rrType,
				"question": question.Name,
				"source":   "client-recurse",
				"host":     addr,
			}).Info("Sent recursive response")

			return
		} else {
			log.WithFields(log.Fields{
				"client":   clientIp,
				"type":     rrType,
				"question": question.Name,
				"source":   "default-recurse",
				"host":     addr,
			}).Warn("Recurser error:", err)
		}
	}

	// I give up
	log.WithFields(log.Fields{
		"client":   clientIp,
		"type":     rrType,
		"question": question.Name,
	}).Warn("No answer found")
	dns.HandleFailed(w, req)
}
