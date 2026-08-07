package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	dockerSock = "/var/run/docker.sock"
	apiBase    = "http://docker" // no version prefix — uses daemon's native API version
)

type appConfig struct {
	Secret       string
	Image        string
	SlackWebhook string
	Port         string
}

var cfg appConfig
var dockerHTTP *http.Client

func main() {
	cfg = appConfig{
		Secret:       os.Getenv("DOCKHOOK_SECRET"),
		Image:        os.Getenv("DOCKHOOK_IMAGE"),
		SlackWebhook: os.Getenv("DOCKHOOK_SLACK_WEBHOOK"),
		Port:         getenv("DOCKHOOK_PORT", "8080"),
	}
	if cfg.Secret == "" || cfg.Image == "" {
		log.Fatal("DOCKHOOK_SECRET and DOCKHOOK_IMAGE are required")
	}

	dockerHTTP = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", dockerSock)
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", handleWebhook)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	log.Printf("dockhook listening on :%s watching %s", cfg.Port, cfg.Image)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, mux))
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	secret := r.Header.Get("X-Webhook-Secret")
	if secret == "" {
		secret = r.URL.Query().Get("secret")
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(cfg.Secret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprint(w, `{"status":"accepted"}`)
	go deploy()
}

func deploy() {
	ctx := context.Background()

	// Snapshot running containers before pull (image digest changes after pull)
	targets, err := listContainersForImage(ctx, cfg.Image)
	if err != nil {
		notify(false, fmt.Sprintf("list containers: %v", err))
		return
	}
	if len(targets) == 0 {
		notify(false, fmt.Sprintf("no running containers found for image %s", cfg.Image))
		return
	}

	log.Printf("pulling %s", cfg.Image)
	if err := pullImage(ctx, cfg.Image); err != nil {
		notify(false, fmt.Sprintf("pull %s: %v", cfg.Image, err))
		return
	}
	log.Printf("pull complete: %s", cfg.Image)

	var updated, failed []string
	for _, c := range targets {
		name := containerName(c)
		log.Printf("recreating %s (%s)", name, c.ID[:12])
		if err := recreate(ctx, c.ID); err != nil {
			log.Printf("recreate %s: %v", name, err)
			failed = append(failed, name)
		} else {
			updated = append(updated, name)
		}
	}

	if len(failed) > 0 {
		notify(false, fmt.Sprintf("%s: updated %v, failed %v", cfg.Image, updated, failed))
	} else {
		notify(true, fmt.Sprintf("updated %s, restarted: %s", cfg.Image, strings.Join(updated, ", ")))
	}
}

// ---- Docker REST API types (minimal) ----

type containerSummary struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
}

type containerDetail struct {
	Name            string          `json:"Name"`
	Config          json.RawMessage `json:"Config"`
	HostConfig      json.RawMessage `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
}

// ---- Docker REST API calls ----

func listContainersForImage(ctx context.Context, imageName string) ([]containerSummary, error) {
	data, status, err := dockerCall(ctx, "GET", "/containers/json", nil, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("status %d: %s", status, data)
	}
	var all []containerSummary
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	var matched []containerSummary
	for _, c := range all {
		if imageMatches(c.Image, imageName) {
			matched = append(matched, c)
		}
	}
	return matched, nil
}

func pullImage(ctx context.Context, imageName string) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		apiBase+"/images/create?fromImage="+imageName, nil)
	if err != nil {
		return err
	}
	resp, err := dockerHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return nil
}

func inspectContainer(ctx context.Context, id string) (*containerDetail, error) {
	data, status, err := dockerCall(ctx, "GET", "/containers/"+id+"/json", nil, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("status %d: %s", status, data)
	}
	var detail containerDetail
	return &detail, json.Unmarshal(data, &detail)
}

func stopContainer(ctx context.Context, id string) error {
	data, status, err := dockerCall(ctx, "POST", "/containers/"+id+"/stop?t=10", nil, "")
	if err != nil {
		return err
	}
	if status != 204 && status != 304 {
		return fmt.Errorf("status %d: %s", status, data)
	}
	return nil
}

func removeContainer(ctx context.Context, id string) error {
	data, status, err := dockerCall(ctx, "DELETE", "/containers/"+id, nil, "")
	if err != nil {
		return err
	}
	if status != 204 {
		return fmt.Errorf("status %d: %s", status, data)
	}
	return nil
}

func createContainer(ctx context.Context, name string, body []byte) (string, error) {
	data, status, err := dockerCall(ctx, "POST", "/containers/create?name="+name,
		bytes.NewReader(body), "application/json")
	if err != nil {
		return "", err
	}
	if status != 201 {
		return "", fmt.Errorf("status %d: %s", status, data)
	}
	var resp struct {
		ID string `json:"Id"`
	}
	return resp.ID, json.Unmarshal(data, &resp)
}

func startContainer(ctx context.Context, id string) error {
	data, status, err := dockerCall(ctx, "POST", "/containers/"+id+"/start", nil, "")
	if err != nil {
		return err
	}
	if status != 204 && status != 304 {
		return fmt.Errorf("status %d: %s", status, data)
	}
	return nil
}

func connectNetwork(ctx context.Context, network, containerID string, ep json.RawMessage) error {
	body, _ := json.Marshal(map[string]any{
		"Container":      containerID,
		"EndpointConfig": ep,
	})
	data, status, err := dockerCall(ctx, "POST", "/networks/"+network+"/connect",
		bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("status %d: %s", status, data)
	}
	return nil
}

func recreate(ctx context.Context, id string) error {
	detail, err := inspectContainer(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	name := strings.TrimPrefix(detail.Name, "/")

	if err := stopContainer(ctx, id); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if err := removeContainer(ctx, id); err != nil {
		return fmt.Errorf("remove: %w", err)
	}

	// Build create body: Config fields at top level + HostConfig + NetworkingConfig.
	// Docker's create API is a flat merge of config fields with HostConfig nested.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(detail.Config, &body); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	body["HostConfig"] = detail.HostConfig

	// Docker only accepts one network at container creation; connect extras after.
	var firstNet string
	var extraNets []string
	for n := range detail.NetworkSettings.Networks {
		if firstNet == "" {
			firstNet = n
		} else {
			extraNets = append(extraNets, n)
		}
	}
	if firstNet != "" {
		netCfg, _ := json.Marshal(map[string]any{
			"EndpointsConfig": map[string]json.RawMessage{
				firstNet: detail.NetworkSettings.Networks[firstNet],
			},
		})
		body["NetworkingConfig"] = netCfg
	}

	createBody, _ := json.Marshal(body)
	newID, err := createContainer(ctx, name, createBody)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}

	for _, netName := range extraNets {
		ep := detail.NetworkSettings.Networks[netName]
		if err := connectNetwork(ctx, netName, newID, ep); err != nil {
			log.Printf("warning: connect %s to network %s: %v", name, netName, err)
		}
	}

	if err := startContainer(ctx, newID); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	log.Printf("recreated %s → %s", name, newID[:12])
	return nil
}

// ---- helpers ----

func dockerCall(ctx context.Context, method, path string, body io.Reader, ct string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, body)
	if err != nil {
		return nil, 0, err
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := dockerHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

func imageMatches(containerImage, target string) bool {
	norm := func(s string) string {
		s = strings.TrimPrefix(s, "docker.io/")
		s = strings.TrimPrefix(s, "library/")
		if i := strings.Index(s, "@"); i != -1 {
			s = s[:i]
		}
		if !strings.Contains(s, ":") {
			s += ":latest"
		}
		return s
	}
	return norm(containerImage) == norm(target)
}

func containerName(c containerSummary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID[:12]
}

func notify(success bool, msg string) {
	log.Printf("result success=%v: %s", success, msg)
	if cfg.SlackWebhook == "" {
		return
	}
	icon := ":white_check_mark:"
	if !success {
		icon = ":x:"
	}
	payload, _ := json.Marshal(map[string]string{
		"text": fmt.Sprintf("%s *dockhook* %s", icon, msg),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.SlackWebhook, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("slack notify: %v", err)
		return
	}
	resp.Body.Close()
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
