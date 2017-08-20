package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/hashicorp/consul/api"
	cons "github.com/myENA/consultant"
	"github.com/nathanejohnson/conseption/putbackreader"
)

var precomma = regexp.MustCompile(`^\s*,`)

// ConsulPort - the tcp port consul agents listen on.
const ConsulPort = 8500

func main() {

	cspt := conseption{
		conf: api.DefaultConfig(),
		me:   os.Getenv("HOSTNAME"),
	}
	var err error

	fmt.Printf("me: %s\n", cspt.me)
	cspt.cc, err = cons.NewClient(cspt.conf)

	if err != nil {
		fatalf("Error connecting to consul: %s\n", err)
		os.Exit(1)
	}

	// make prefix configurable
	wkp, err := cspt.cc.WatchKeyPrefix("/services", true, cspt.handler)
	if err != nil {
		fatalf("Error setting up watcher: %s\n", err)
	}
	svcs, _, err := cspt.cc.Catalog().Services(&api.QueryOptions{AllowStale: true})
	if err != nil {
		fatalf("Error querying catalog: %s\n", err)
	}

	// Go through all services in the catalog, and deregister anything that
	// should go to the local host agent.
	for k := range svcs {
		css, _, err := cspt.cc.Catalog().Service(k, "", &api.QueryOptions{AllowStale: true})
		if err != nil {
			fmt.Printf("got error querying catalog: %s\n", err)
		}
		for _, cs := range css {
			if cs.ServiceAddress == cspt.me && cs.Node != cspt.me {
				err = cspt.deregRemote(cs)
				if err != nil {
					fmt.Printf("got error derigstering: %s\n", err)
				}
			}
		}
	}

	// Start the runner, which will get an initial full kv dump of everything
	// under the kv prefix.
	err = wkp.Run(cspt.conf.Address)
	if err != nil {
		fatalf("%s\n", err)
	}

}

type conseption struct {
	conf *api.Config
	cc   *cons.Client
	me   string
}

type services struct {
	Services []*api.AgentServiceRegistration
}

func (cspt *conseption) deregRemote(se *api.CatalogService) error {
	// shallow copy of conf
	conf := &api.Config{}
	*conf = *cspt.conf
	// get info on the node
	cn, _, err := cspt.cc.Catalog().Node(se.Node, &api.QueryOptions{AllowStale: true})
	if err != nil {
		return err
	}
	conf.Address = fmt.Sprintf("%s:%d", cn.Node.Address, ConsulPort)

	rcc, err := cons.NewClient(conf)
	if err != nil {
		return err
	}
	fmt.Printf("deregistering %s from %s\n", se.ServiceName, se.Node)

	return rcc.Agent().ServiceDeregister(se.ServiceID)
}

func (cspt *conseption) handler(idx uint64, raw interface{}) {
	kvps, ok := raw.(api.KVPairs)
	var svcs []*api.AgentServiceRegistration
	if !ok {
		fmt.Println("not KVPairs!")
		return
	}
	for _, kvp := range kvps {
		s, err := parseServiceRegs(kvp.Value)
		if err != nil {
			fmt.Printf("error parsing service reg: %s\n", err)
			if s == nil {
				return
			}
		}
		for _, svc := range s {
			if svc.Address == cspt.me {
				svcs = append(svcs, svc)
			}
		}
	}
	err := cspt.deregisterAllLocalServices()
	if err != nil {
		fmt.Printf("error deregistering services: %s\n", err)
	}
	for _, svc := range svcs {
		var err error
		fmt.Printf("I'm totally registering %s\n", svc.ID)
		err = cspt.cc.Agent().ServiceRegister(svc)
		if err != nil {
			fmt.Printf("error returned from registering service: %s\n", err)
		}
	}
}

func parseServiceRegs(val []byte) ([]*api.AgentServiceRegistration, error) {
	var errors []string
	var err error
	// Try services struct
	ss := &services{}
	err = json.Unmarshal(val, ss)
	if err == nil {
		return ss.Services, nil
	}

	// now try a list
	err = json.Unmarshal(val, &ss.Services)
	if err == nil {
		return ss.Services, nil
	}

	// now try comma separated json objects.
	pbr := putbackreader.NewPutBackReader(bytes.NewReader(val))
	jd := json.NewDecoder(pbr)
	buf := new(bytes.Buffer)

	for {
		s := &api.AgentServiceRegistration{}
		err = jd.Decode(s)

		if err != nil {
			if err == io.EOF {
				break
			}
			// Handle the case where we have comma separated json
			// objects.
			buf.Reset()
			_, _ = buf.ReadFrom(jd.Buffered())
			b := buf.Bytes()
			m := precomma.FindIndex(b)
			if m == nil {
				errors = append(errors, fmt.Sprintf("bad read: %s\n", string(b)))
				break
			}

			// Take the comma off, put the already-read parts of the stream
			// back, and make a new decoder.  All this work to subtract
			// a fucking wayward comma from the stream.
			pbr.SetBackBytes(b[m[1]:])
			jd = json.NewDecoder(pbr)

			err = jd.Decode(s)
			if err != nil {
				if err == io.EOF {
					err = nil
				} else {
					errors = append(errors, fmt.Sprintf("got final error: %s\n", err))
				}
				break
			}
		}
		ss.Services = append(ss.Services, s)

		if !jd.More() {
			break
		}
	}

	if len(errors) > 0 {
		err = fmt.Errorf("Errors: %s", strings.Join(errors, ","))
	}
	return ss.Services, err
}

func (cspt *conseption) deregisterAllLocalServices() error {
	a := cspt.cc.Agent()
	services, err := a.Services()
	if err != nil {
		return err
	}
	var errs []string
	for _, s := range services {
		err = a.ServiceDeregister(s.ID)
		fmt.Printf("deregistering %s\n", s.ID)
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if errs != nil {
		return fmt.Errorf("errors deregistering service: %s", strings.Join(errs, ","))
	}
	return nil
}

func fatalf(format string, args ...interface{}) {
	fmt.Printf(format, args)
	os.Exit(1)
}
