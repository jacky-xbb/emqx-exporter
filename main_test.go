// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"emqx-exporter/config"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/valyala/fasthttp"
	"gopkg.in/yaml.v3"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

var ctx = context.Background()

type testContainer struct {
	id          string
	name        string
	image       string
	hostPortMap map[nat.Port]string
}

var emqxContainer = testContainer{
	name:  "emqx-for-emqx-exporter-test",
	image: "docker.io/emqx/emqx-enterprise:5.3",
	hostPortMap: map[nat.Port]string{
		"18084/tcp": "18084",
		"18083/tcp": "18083",
		"1883/tcp":  "1883",
		"8883/tcp":  "8883",
		"8083/tcp":  "8083",
		"8084/tcp":  "38084", // Github Action will use 8084, so we use 38084
	},
}

type testExporter struct {
	binDir string
	bin    string
	port   int
}

var emqxExporter = &testExporter{
	port: 65534,
}

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	format.MaxLength = 0

	// fetch the current config
	suiteConfig, reporterConfig := GinkgoConfiguration()
	// adjust it
	suiteConfig.SkipStrings = []string{"NEVER-RUN"}
	reporterConfig.FullTrace = true
	// pass it in to RunSpecs
	RunSpecs(t, "Controller Suite", suiteConfig, reporterConfig)
}

var _ = BeforeSuite(func() {
	createEMQXContainer()

	var err error
	binName := "emqx-exporter"

	emqxExporter.binDir, err = os.MkdirTemp("/tmp", binName+"-test-bindir-")
	Expect(err).NotTo(HaveOccurred())
	emqxExporter.bin = emqxExporter.binDir + "/" + binName

	cmd := exec.Command(
		"go",
		"build",
		"-o",
		emqxExporter.bin,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	Expect(cmd.Run()).NotTo(HaveOccurred())

})

var _ = AfterSuite(func() {
	Expect(os.RemoveAll(emqxExporter.binDir)).NotTo(HaveOccurred())
	deleteEMQXContainer()
})

var _ = Describe("EMQX Exporter", func() {
	var cmd *exec.Cmd
	var runningPort int
	var exporterConfig = config.Config{}

	BeforeEach(func() {
		copyCert := exec.Command("cp", "-r", "config/example/certs", emqxExporter.binDir+"/certs")
		Expect(copyCert.Run()).NotTo(HaveOccurred())

		exporterConfig = config.Config{
			Probes: []config.Probe{
				{Target: "127.0.0.1:1883", Scheme: "tcp"},
				{Target: "127.0.0.1:8883", Scheme: "ssl",
					TLSClientConfig: &config.TLSClientConfig{
						InsecureSkipVerify: true,
						CAFile:             emqxExporter.binDir + "/certs/cacert.pem",
						CertFile:           emqxExporter.binDir + "/certs/client-cert.pem",
						KeyFile:            emqxExporter.binDir + "/certs/client-key.pem",
					},
				},
				{Target: "127.0.0.1:8083/mqtt", Scheme: "ws"},
				{Target: "127.0.0.1:38084/mqtt", Scheme: "wss",
					TLSClientConfig: &config.TLSClientConfig{
						InsecureSkipVerify: true,
						CAFile:             emqxExporter.binDir + "/certs/cacert.pem",
						CertFile:           emqxExporter.binDir + "/certs/client-cert.pem",
						KeyFile:            emqxExporter.binDir + "/certs/client-key.pem",
					},
				},
			},
		}

		configFile, _ := yaml.Marshal(exporterConfig)
		configFilePath := emqxExporter.binDir + "/config.yml"
		Expect(os.WriteFile(configFilePath, configFile, 0644)).ToNot(HaveOccurred())

		cmd = exec.CommandContext(ctx, emqxExporter.bin,
			"--web.listen-address", fmt.Sprintf(":%d", emqxExporter.port),
			"--config.file", configFilePath,
		)
		Expect(cmd.Start()).ToNot(HaveOccurred())

		runningPort = emqxExporter.port
		emqxExporter.port--
	})

	AfterEach(func() {
		Expect(os.Remove(emqxExporter.binDir + "/config.yml")).NotTo(HaveOccurred())
		Expect(cmd.Process.Kill()).NotTo(HaveOccurred())
	})

	Context("when the exporter is running", func() {
		DescribeTable("check probe",
			func(target string) {
				uri := &fasthttp.URI{}
				uri.SetScheme("http")
				uri.SetHost("127.0.0.1:" + strconv.Itoa(runningPort))
				uri.SetPath("/probe")
				uri.SetQueryString("target=" + target)

				var mf map[string]*dto.MetricFamily
				Eventually(func() (err error) {
					mf, err = callExporterAPI(uri.String())
					return err
				}).WithTimeout(10 * time.Second).WithPolling(500 * time.Millisecond).ShouldNot(HaveOccurred())

				Expect(mf).Should(And(
					HaveKeyWithValue("emqx_mqtt_probe_duration_seconds", And(
						WithTransform(func(m *dto.MetricFamily) string {
							return m.GetName()
						}, Equal("emqx_mqtt_probe_duration_seconds")),
						WithTransform(func(m *dto.MetricFamily) string {
							return m.GetType().String()
						}, Equal("GAUGE")),
						WithTransform(func(m *dto.MetricFamily) float64 {
							return *m.Metric[0].Gauge.Value
						}, Not(BeZero())),
					)),
					HaveKeyWithValue("emqx_mqtt_probe_success", And(
						WithTransform(func(m *dto.MetricFamily) string {
							return m.GetName()
						}, Equal("emqx_mqtt_probe_success")),
						WithTransform(func(m *dto.MetricFamily) string {
							return m.GetType().String()
						}, Equal("GAUGE")),
						WithTransform(func(m *dto.MetricFamily) int {
							return int(*m.Metric[0].Gauge.Value)
						}, Equal(1)),
					)),
				))

			},
			Entry("mqtt", "127.0.0.1:1883"),
			Entry("ssl", "127.0.0.1:8883"),
			Entry("ws", "127.0.0.1:8083/mqtt"),
			Entry("wss", "127.0.0.1:38084/mqtt"),
		)
	})
})

func callExporterAPI(url string) (map[string]*dto.MetricFamily, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, http.StatusText(resp.StatusCode))
	}

	var parser expfmt.TextParser
	mf, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, err
	}
	return mf, nil
}

func deleteEMQXContainer() {
	cli, err := dockerClient.NewClientWithOpts(
		dockerClient.FromEnv,
		dockerClient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		panic(err)
	}

	if err := cli.ContainerStop(ctx, emqxContainer.id, container.StopOptions{}); err != nil {
		panic(err)
	}
	if err := cli.ContainerRemove(ctx, emqxContainer.id, types.ContainerRemoveOptions{}); err != nil {
		panic(err)
	}
}

func createEMQXContainer() {
	cli, err := dockerClient.NewClientWithOpts(
		dockerClient.FromEnv,
		dockerClient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		panic(err)
	}

	reader, err := cli.ImagePull(ctx, emqxContainer.image, types.ImagePullOptions{})
	if err != nil {
		panic(err)
	}
	defer reader.Close()
	_, _ = io.Copy(io.Discard, reader)

	containerConf := &container.Config{
		Image:        emqxContainer.image,
		ExposedPorts: nat.PortSet{},
		Healthcheck: &container.HealthConfig{
			Test: []string{"CMD", "curl", "-f", "http://localhost:18083/status"},
		},
	}
	containerHostConf := &container.HostConfig{
		PortBindings: nat.PortMap{},
	}

	for containerPort, hostPort := range emqxContainer.hostPortMap {
		containerConf.ExposedPorts[containerPort] = struct{}{}
		containerHostConf.PortBindings[containerPort] = []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: hostPort}}
	}
	resp, err := cli.ContainerCreate(ctx, containerConf, containerHostConf, nil, nil, emqxContainer.name)

	if err != nil {
		panic(err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		panic(err)
	}
	emqxContainer.id = resp.ID

	var i int = 0
	var max int = 60
	for i <= max {
		cont, err := cli.ContainerInspect(ctx, resp.ID)
		if err != nil {
			panic(err)
		}
		if cont.State.Health.Status == "healthy" {
			break
		}
		time.Sleep(1 * time.Second)
		i++
	}

	if i == max {
		panic("EMQX container is not healthy")
	}
}
