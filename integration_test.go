//go:build integration
// +build integration

package headscale

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	"inet.af/netaddr"
)

var (
	integrationTmpDir string
	ih                Headscale
)

var (
	pool           dockertest.Pool
	network        dockertest.Network
	headscale      dockertest.Resource
	tailscaleCount int = 25
	tailscales     map[string]dockertest.Resource
)

var tailscaleVersions = []string{"1.14.3", "1.12.3"}

type IntegrationTestSuite struct {
	suite.Suite
	stats *suite.SuiteInformation
}

func TestIntegrationTestSuite(t *testing.T) {
	s := new(IntegrationTestSuite)
	suite.Run(t, s)

	// HandleStats, which allows us to check if we passed and save logs
	// is called after TearDown, so we cannot tear down containers before
	// we have potentially saved the logs.
	for _, tailscale := range tailscales {
		if err := pool.Purge(&tailscale); err != nil {
			log.Printf("Could not purge resource: %s\n", err)
		}
	}

	if !s.stats.Passed() {
		err := saveLog(&headscale, "test_output")
		if err != nil {
			log.Printf("Could not save log: %s\n", err)
		}
	}
	if err := pool.Purge(&headscale); err != nil {
		log.Printf("Could not purge resource: %s\n", err)
	}

	if err := network.Close(); err != nil {
		log.Printf("Could not close network: %s\n", err)
	}
}

func executeCommand(resource *dockertest.Resource, cmd []string) (string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode, err := resource.Exec(
		cmd,
		dockertest.ExecOptions{
			StdOut: &stdout,
			StdErr: &stderr,
		},
	)
	if err != nil {
		return "", err
	}

	if exitCode != 0 {
		fmt.Println("Command: ", cmd)
		fmt.Println("stdout: ", stdout.String())
		fmt.Println("stderr: ", stderr.String())
		return "", fmt.Errorf("command failed with: %s", stderr.String())
	}

	return stdout.String(), nil
}

func saveLog(resource *dockertest.Resource, basePath string) error {
	err := os.MkdirAll(basePath, os.ModePerm)
	if err != nil {
		return err
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err = pool.Client.Logs(
		docker.LogsOptions{
			Context:      context.TODO(),
			Container:    resource.Container.ID,
			OutputStream: &stdout,
			ErrorStream:  &stderr,
			Tail:         "all",
			RawTerminal:  false,
			Stdout:       true,
			Stderr:       true,
			Follow:       false,
			Timestamps:   false,
		},
	)
	if err != nil {
		return err
	}

	fmt.Printf("Saving logs for %s to %s\n", resource.Container.Name, basePath)

	err = ioutil.WriteFile(path.Join(basePath, resource.Container.Name+".stdout.log"), []byte(stdout.String()), 0o644)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(path.Join(basePath, resource.Container.Name+".stderr.log"), []byte(stdout.String()), 0o644)
	if err != nil {
		return err
	}

	return nil
}

func dockerRestartPolicy(config *docker.HostConfig) {
	// set AutoRemove to true so that stopped container goes away by itself
	config.AutoRemove = true
	config.RestartPolicy = docker.RestartPolicy{
		Name: "no",
	}
}

func tailscaleContainer(identifier string, version string) (string, *dockertest.Resource) {
	tailscaleBuildOptions := &dockertest.BuildOptions{
		Dockerfile: "Dockerfile.tailscale",
		ContextDir: ".",
		BuildArgs: []docker.BuildArg{
			{
				Name:  "TAILSCALE_VERSION",
				Value: version,
			},
		},
	}
	hostname := fmt.Sprintf("tailscale-%s-%s", strings.Replace(version, ".", "-", -1), identifier)
	tailscaleOptions := &dockertest.RunOptions{
		Name:     hostname,
		Networks: []*dockertest.Network{&network},
		Cmd:      []string{"tailscaled", "--tun=userspace-networking", "--socks5-server=localhost:1055"},
	}

	pts, err := pool.BuildAndRunWithBuildOptions(tailscaleBuildOptions, tailscaleOptions, dockerRestartPolicy)
	if err != nil {
		log.Fatalf("Could not start resource: %s", err)
	}
	fmt.Printf("Created %s container\n", hostname)
	return hostname, pts
}

func (s *IntegrationTestSuite) SetupSuite() {
	var err error
	h = Headscale{
		dbType:   "sqlite3",
		dbString: "integration_test_db.sqlite3",
	}

	if ppool, err := dockertest.NewPool(""); err == nil {
		pool = *ppool
	} else {
		log.Fatalf("Could not connect to docker: %s", err)
	}

	if pnetwork, err := pool.CreateNetwork("headscale-test"); err == nil {
		network = *pnetwork
	} else {
		log.Fatalf("Could not create network: %s", err)
	}

	headscaleBuildOptions := &dockertest.BuildOptions{
		Dockerfile: "Dockerfile",
		ContextDir: ".",
	}

	currentPath, err := os.Getwd()
	if err != nil {
		log.Fatalf("Could not determine current path: %s", err)
	}

	headscaleOptions := &dockertest.RunOptions{
		Name: "headscale",
		Mounts: []string{
			fmt.Sprintf("%s/integration_test/etc:/etc/headscale", currentPath),
			fmt.Sprintf("%s/derp.yaml:/etc/headscale/derp.yaml", currentPath),
		},
		Networks: []*dockertest.Network{&network},
		Cmd:      []string{"headscale", "serve"},
		PortBindings: map[docker.Port][]docker.PortBinding{
			"8080/tcp": {{HostPort: "8080"}},
		},
	}

	fmt.Println("Creating headscale container")
	if pheadscale, err := pool.BuildAndRunWithBuildOptions(headscaleBuildOptions, headscaleOptions, dockerRestartPolicy); err == nil {
		headscale = *pheadscale
	} else {
		log.Fatalf("Could not start resource: %s", err)
	}
	fmt.Println("Created headscale container")

	fmt.Println("Creating tailscale containers")
	tailscales = make(map[string]dockertest.Resource)
	for i := 0; i < tailscaleCount; i++ {
		version := tailscaleVersions[i%len(tailscaleVersions)]

		hostname, container := tailscaleContainer(fmt.Sprint(i), version)
		tailscales[hostname] = *container
	}

	fmt.Println("Waiting for headscale to be ready")
	hostEndpoint := fmt.Sprintf("localhost:%s", headscale.GetPort("8080/tcp"))

	if err := pool.Retry(func() error {
		url := fmt.Sprintf("http://%s/health", hostEndpoint)
		resp, err := http.Get(url)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status code not OK")
		}
		return nil
	}); err != nil {
		log.Fatalf("Could not connect to docker: %s", err)
	}
	fmt.Println("headscale container is ready")

	fmt.Println("Creating headscale namespace")
	result, err := executeCommand(
		&headscale,
		[]string{"headscale", "namespaces", "create", "test"},
	)
	assert.Nil(s.T(), err)

	fmt.Println("Creating pre auth key")
	authKey, err := executeCommand(
		&headscale,
		[]string{"headscale", "-n", "test", "preauthkeys", "create", "--reusable", "--expiration", "24h"},
	)
	assert.Nil(s.T(), err)

	headscaleEndpoint := fmt.Sprintf("http://headscale:%s", headscale.GetPort("8080/tcp"))

	fmt.Printf("Joining tailscale containers to headscale at %s\n", headscaleEndpoint)
	for hostname, tailscale := range tailscales {
		command := []string{"tailscale", "up", "-login-server", headscaleEndpoint, "--authkey", strings.TrimSuffix(authKey, "\n"), "--hostname", hostname}

		fmt.Println("Join command:", command)
		fmt.Printf("Running join command for %s\n", hostname)
		result, err = executeCommand(
			&tailscale,
			command,
		)
		fmt.Println("tailscale result: ", result)
		assert.Nil(s.T(), err)
		fmt.Printf("%s joined\n", hostname)
	}

	// The nodes need a bit of time to get their updated maps from headscale
	// TODO: See if we can have a more deterministic wait here.
	time.Sleep(60 * time.Second)
}

func (s *IntegrationTestSuite) TearDownSuite() {
}

func (s *IntegrationTestSuite) HandleStats(suiteName string, stats *suite.SuiteInformation) {
	s.stats = stats
}

func (s *IntegrationTestSuite) TestListNodes() {
	fmt.Println("Listing nodes")
	result, err := executeCommand(
		&headscale,
		[]string{"headscale", "-n", "test", "nodes", "list"},
	)
	assert.Nil(s.T(), err)

	fmt.Printf("List nodes: \n%s\n", result)

	// Chck that the correct count of host is present in node list
	lines := strings.Split(result, "\n")
	assert.Equal(s.T(), len(tailscales), len(lines)-2)

	for hostname := range tailscales {
		assert.Contains(s.T(), result, hostname)
	}
}

func (s *IntegrationTestSuite) TestGetIpAddresses() {
	ipPrefix := netaddr.MustParseIPPrefix("100.64.0.0/10")
	ips, err := getIPs()
	assert.Nil(s.T(), err)

	for hostname := range tailscales {
		s.T().Run(hostname, func(t *testing.T) {
			ip := ips[hostname]

			fmt.Printf("IP for %s: %s\n", hostname, ip)

			// c.Assert(ip.Valid(), check.IsTrue)
			assert.True(t, ip.Is4())
			assert.True(t, ipPrefix.Contains(ip))

			ips[hostname] = ip
		})
	}
}

func (s *IntegrationTestSuite) TestStatus() {
	ips, err := getIPs()
	assert.Nil(s.T(), err)

	for hostname, tailscale := range tailscales {
		s.T().Run(hostname, func(t *testing.T) {
			command := []string{"tailscale", "status"}

			fmt.Printf("Getting status for %s\n", hostname)
			result, err := executeCommand(
				&tailscale,
				command,
			)
			assert.Nil(t, err)
			// fmt.Printf("Status for %s: %s", hostname, result)

			// Check if we have as many nodes in status
			// as we have IPs/tailscales
			lines := strings.Split(result, "\n")
			assert.Equal(t, len(ips), len(lines)-1)
			assert.Equal(t, len(tailscales), len(lines)-1)

			// Check that all hosts is present in all hosts status
			for ipHostname, ip := range ips {
				assert.Contains(t, result, ip.String())
				assert.Contains(t, result, ipHostname)
			}
		})
	}
}

func (s *IntegrationTestSuite) TestPingAllPeers() {
	ips, err := getIPs()
	assert.Nil(s.T(), err)

	for hostname, tailscale := range tailscales {
		for peername, ip := range ips {
			s.T().Run(fmt.Sprintf("%s-%s", hostname, peername), func(t *testing.T) {
				// We currently cant ping ourselves, so skip that.
				if peername != hostname {
					// We are only interested in "direct ping" which means what we
					// might need a couple of more attempts before reaching the node.
					command := []string{
						"tailscale", "ping",
						"--timeout=1s",
						"--c=20",
						"--until-direct=true",
						ip.String(),
					}

					fmt.Printf("Pinging from %s (%s) to %s (%s)\n", hostname, ips[hostname], peername, ip)
					result, err := executeCommand(
						&tailscale,
						command,
					)
					assert.Nil(t, err)
					fmt.Printf("Result for %s: %s\n", hostname, result)
					assert.Contains(t, result, "pong")
				}
			})
		}
	}
}

func getIPs() (map[string]netaddr.IP, error) {
	ips := make(map[string]netaddr.IP)
	for hostname, tailscale := range tailscales {
		command := []string{"tailscale", "ip"}

		result, err := executeCommand(
			&tailscale,
			command,
		)
		if err != nil {
			return nil, err
		}

		ip, err := netaddr.ParseIP(strings.TrimSuffix(result, "\n"))
		if err != nil {
			return nil, err
		}

		ips[hostname] = ip
	}
	return ips, nil
}
