/*
Copyright 2014 Rohith All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package marathon

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	memberStatusUp   = 0
	memberStatusDown = 1
)

// the status of a member node
type memberStatus int

// cluster is a collection of marathon nodes
type cluster struct {
	sync.RWMutex
	// a collection of nodes
	members []*member
	// the marathon HTTP client to ensure consistency in requests
	client *httpClient
	// healthCheckInterval is the interval by which we probe down nodes for
	// availability again.
	healthCheckInterval time.Duration
	// done is a channel signaling to all pending health-checking routines
	// that it's time to shut down.
	done chan struct{}
	// isDone is used to guarantee thread-safety when calling Stop().
	isDone bool
	// healthCheckWg is a sync.Workgroup sychronizing the successful
	// termination of all pending health-check routines.
	healthCheckWg sync.WaitGroup
}

// member represents an individual endpoint
type member struct {
	// the name / ip address of the host
	endpoint string
	// the status of the host
	status memberStatus
}

// newCluster returns a new marathon cluster
func newCluster(client *httpClient, marathonURL string, isDCOS bool) (*cluster, error) {
	// step: extract and basic validate the endpoints
	var members []*member
	var defaultProto string

	for _, endpoint := range strings.Split(marathonURL, ",") {
		// step: check for nothing
		if endpoint == "" {
			return nil, newInvalidEndpointError("endpoint is blank")
		}
		// step: prepend scheme if missing on (non-initial) endpoint.
		if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
			if defaultProto == "" {
				return nil, newInvalidEndpointError("missing scheme on (first) endpoint")
			}

			endpoint = fmt.Sprintf("%s://%s", defaultProto, endpoint)
		}
		// step: parse the url
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, newInvalidEndpointError("invalid endpoint '%s': %s", endpoint, err)
		}
		if defaultProto == "" {
			defaultProto = u.Scheme
		}

		// step: check for empty hosts
		if u.Host == "" {
			return nil, newInvalidEndpointError("endpoint: %s must have a host", endpoint)
		}

		// step: if DCOS is set and no path is given, set the default DCOS path.
		// done in order to maintain compatibility with automatic addition of the
		// default DCOS path.
		if isDCOS && strings.TrimLeft(u.Path, "/") == "" {
			u.Path = defaultDCOSPath
		}

		// step: create a new node for this endpoint
		members = append(members, &member{endpoint: u.String()})
	}

	return &cluster{
		client:              client,
		members:             members,
		healthCheckInterval: 5 * time.Second,
		done:                make(chan struct{}),
	}, nil
}

// Stop gracefully terminates the cluster. It returns once all health-checking
// goroutines have finished.
func (c *cluster) Stop() {
	c.Lock()
	defer c.Unlock()
	if c.isDone {
		return
	}
	c.isDone = true
	close(c.done)
	c.healthCheckWg.Wait()
}

// retrieve the current member, i.e. the current endpoint in use
func (c *cluster) getMember() (string, error) {
	c.RLock()
	defer c.RUnlock()
	for _, n := range c.members {
		if n.status == memberStatusUp {
			return n.endpoint, nil
		}
	}

	return "", ErrMarathonDown
}

// markDown marks down the current endpoint
func (c *cluster) markDown(endpoint string) {
	c.Lock()
	defer c.Unlock()
	for _, n := range c.members {
		// step: check if this is the node and it's marked as up - The double  checking on the
		// nodes status ensures the multiple calls don't create multiple checks
		if n.status == memberStatusUp && n.endpoint == endpoint {
			n.status = memberStatusDown
			c.healthCheckWg.Add(1)
			go func() {
				defer c.healthCheckWg.Done()
				c.healthCheckNode(n)
			}()
			break
		}
	}
}

// healthCheckNode performs a health check on the node and when active updates the status
func (c *cluster) healthCheckNode(node *member) {
	// step: wait for the node to become active ... we are assuming a /ping is enough here
	ticker := time.NewTicker(c.healthCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			req, err := c.client.buildMarathonRequest("GET", node.endpoint, "ping", nil)
			if err == nil {
				res, err := c.client.Do(req)
				if err == nil && res.StatusCode == 200 {
					// step: mark the node as active again
					c.Lock()
					node.status = memberStatusUp
					c.Unlock()
					break
				}
			}
		}
	}
}

// activeMembers returns a list of active members
func (c *cluster) activeMembers() []string {
	return c.membersList(memberStatusUp)
}

// nonActiveMembers returns a list of non-active members in the cluster
func (c *cluster) nonActiveMembers() []string {
	return c.membersList(memberStatusDown)
}

// memberList returns a list of members of a specified status
func (c *cluster) membersList(status memberStatus) []string {
	c.RLock()
	defer c.RUnlock()
	var list []string
	for _, m := range c.members {
		if m.status == status {
			list = append(list, m.endpoint)
		}
	}

	return list
}

// size returns the size of the cluster
func (c *cluster) size() int {
	return len(c.members)
}

// String returns a string representation
func (m member) String() string {
	status := "UP"
	if m.status == memberStatusDown {
		status = "DOWN"
	}

	return fmt.Sprintf("member: %s:%s", m.endpoint, status)
}
