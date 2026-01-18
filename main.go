package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type ValError struct {
	File string
	Line int // 0 => line not required/unknown
	Msg  string
}

func (e ValError) String() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d %s", e.File, e.Line, e.Msg)
	}
	return e.Msg
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: yamlvalid <path-to-yaml>")
		os.Exit(2)
	}

	path := os.Args[1]
	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read file content: %v\n", err)
		os.Exit(1)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		fmt.Fprintf(os.Stderr, "cannot unmarshal file content: %v\n", err)
		os.Exit(1)
	}

	errs := validatePod(filepath.Clean(path), &root)
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, e.String())
		}
		os.Exit(1)
	}

	os.Exit(0)
}

func validatePod(file string, root *yaml.Node) []ValError {
	doc := firstDocMapping(root)
	if doc == nil {
		return []ValError{{File: file, Msg: "cannot unmarshal file content: empty document"}}
	}

	var errs []ValError

	// Top-level required
	apiV := get(doc, "apiVersion")
	kind := get(doc, "kind")
	meta := get(doc, "metadata")
	spec := get(doc, "spec")

	if apiV == nil {
		errs = append(errs, req(file, "apiVersion"))
	} else {
		errs = append(errs, mustString(file, apiV, "apiVersion")...)
		if apiV != nil && apiV.Kind == yaml.ScalarNode && apiV.Tag == "!!str" && apiV.Value != "v1" {
			errs = append(errs, lineErr(file, apiV.Line, fmt.Sprintf("apiVersion has unsupported value '%s'", apiV.Value)))
		}
	}

	if kind == nil {
		errs = append(errs, req(file, "kind"))
	} else {
		errs = append(errs, mustString(file, kind, "kind")...)
		if kind != nil && kind.Kind == yaml.ScalarNode && kind.Tag == "!!str" && kind.Value != "Pod" {
			errs = append(errs, lineErr(file, kind.Line, fmt.Sprintf("kind has unsupported value '%s'", kind.Value)))
		}
	}

	if meta == nil {
		errs = append(errs, req(file, "metadata"))
	} else {
		errs = append(errs, validateObjectMeta(file, meta)...)
	}

	if spec == nil {
		errs = append(errs, req(file, "spec"))
	} else {
		errs = append(errs, validatePodSpec(file, spec)...)
	}

	return errs
}

// ---- ObjectMeta ----

func validateObjectMeta(file string, n *yaml.Node) []ValError {
	if n.Kind != yaml.MappingNode {
		return []ValError{lineErr(file, n.Line, "metadata must be object")}
	}

	var errs []ValError

	name := get(n, "name")
	if name == nil {
		errs = append(errs, req(file, "metadata.name"))
	} else {
		errs = append(errs, mustString(file, name, "metadata.name")...)
	}

	// namespace optional
	if ns := get(n, "namespace"); ns != nil {
		errs = append(errs, mustString(file, ns, "metadata.namespace")...)
	}

	// labels optional: object of string->string
	if labels := get(n, "labels"); labels != nil {
		if labels.Kind != yaml.MappingNode {
			errs = append(errs, lineErr(file, labels.Line, "metadata.labels must be object"))
		} else {
			for i := 0; i < len(labels.Content); i += 2 {
				k := labels.Content[i]
				v := labels.Content[i+1]
				if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
					errs = append(errs, lineErr(file, k.Line, "metadata.labels must be object"))
					break
				}
				if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
					errs = append(errs, lineErr(file, v.Line, "metadata.labels must be object"))
					break
				}
			}
		}
	}

	return errs
}

// ---- PodSpec ----

func validatePodSpec(file string, n *yaml.Node) []ValError {
	if n.Kind != yaml.MappingNode {
		return []ValError{lineErr(file, n.Line, "spec must be object")}
	}

	var errs []ValError

	// os optional: support scalar os: linux/windows OR object os: {name: ...}
	if osNode := get(n, "os"); osNode != nil {
		errs = append(errs, validatePodOS(file, osNode)...)
	}

	containers := get(n, "containers")
	if containers == nil {
		errs = append(errs, req(file, "spec.containers"))
		return errs
	}
	errs = append(errs, validateContainers(file, containers)...)

	return errs
}

// ---- PodOS ----

func validatePodOS(file string, n *yaml.Node) []ValError {
	allowed := map[string]bool{"linux": true, "windows": true}

	// scalar
	if n.Kind == yaml.ScalarNode {
		if n.Tag != "!!str" {
			return []ValError{lineErr(file, n.Line, "os must be string")}
		}
		if !allowed[n.Value] {
			return []ValError{lineErr(file, n.Line, fmt.Sprintf("os has unsupported value '%s'", n.Value))}
		}
		return nil
	}

	// object
	if n.Kind != yaml.MappingNode {
		return []ValError{lineErr(file, n.Line, "os must be object")}
	}
	name := get(n, "name")
	if name == nil {
		return []ValError{req(file, "os.name")}
	}
	if name.Kind != yaml.ScalarNode || name.Tag != "!!str" {
		return []ValError{lineErr(file, name.Line, "os.name must be string")}
	}
	if !allowed[name.Value] {
		return []ValError{lineErr(file, name.Line, fmt.Sprintf("os.name has unsupported value '%s'", name.Value))}
	}
	return nil
}

// ---- Containers ----

var snakeCaseRe = regexp.MustCompile(`^[a-z]+(_[a-z0-9]+)*$`)
var memRe = regexp.MustCompile(`^[0-9]+(Ki|Mi|Gi)$`)

func validateContainers(file string, n *yaml.Node) []ValError {
	if n.Kind != yaml.SequenceNode {
		return []ValError{lineErr(file, n.Line, "spec.containers must be array")}
	}
	if len(n.Content) == 0 {
		return []ValError{req(file, "spec.containers")}
	}

	var errs []ValError
	seen := map[string]bool{}

	for _, c := range n.Content {
		if c.Kind != yaml.MappingNode {
			errs = append(errs, lineErr(file, c.Line, "containers must be object"))
			continue
		}

		// name required
		cname := get(c, "name")
		if cname == nil {
			errs = append(errs, req(file, "containers.name"))
		} else if cname.Kind != yaml.ScalarNode || cname.Tag != "!!str" {
			errs = append(errs, lineErr(file, cname.Line, "containers.name must be string"))
		} else {
			if !snakeCaseRe.MatchString(cname.Value) || seen[cname.Value] {
				errs = append(errs, lineErr(file, cname.Line, fmt.Sprintf("containers.name has invalid format '%s'", cname.Value)))
			}
			seen[cname.Value] = true
		}

		// image required
		img := get(c, "image")
		if img == nil {
			errs = append(errs, req(file, "containers.image"))
		} else if img.Kind != yaml.ScalarNode || img.Tag != "!!str" {
			errs = append(errs, lineErr(file, img.Line, "containers.image must be string"))
		} else if !validImage(img.Value) {
			errs = append(errs, lineErr(file, img.Line, fmt.Sprintf("containers.image has invalid format '%s'", img.Value)))
		}

		// ports optional
		if ports := get(c, "ports"); ports != nil {
			errs = append(errs, validatePorts(file, ports)...)
		}

		// probes optional
		if rp := get(c, "readinessProbe"); rp != nil {
			errs = append(errs, validateProbe(file, rp, "readinessProbe")...)
		}
		if lp := get(c, "livenessProbe"); lp != nil {
			errs = append(errs, validateProbe(file, lp, "livenessProbe")...)
		}

		// resources required
		res := get(c, "resources")
		if res == nil {
			errs = append(errs, req(file, "resources"))
		} else {
			errs = append(errs, validateResources(file, res)...)
		}
	}

	return errs
}

// ---- Ports ----

func validatePorts(file string, n *yaml.Node) []ValError {
	if n.Kind != yaml.SequenceNode {
		return []ValError{lineErr(file, n.Line, "ports must be array")}
	}

	var errs []ValError
	for _, p := range n.Content {
		if p.Kind != yaml.MappingNode {
			errs = append(errs, lineErr(file, p.Line, "ports must be object"))
			continue
		}

		cp := get(p, "containerPort")
		if cp == nil {
			errs = append(errs, req(file, "containerPort"))
		} else {
			errs = append(errs, validatePort(file, cp, "containerPort")...)
		}

		if proto := get(p, "protocol"); proto != nil {
			if proto.Kind != yaml.ScalarNode || proto.Tag != "!!str" {
				errs = append(errs, lineErr(file, proto.Line, "protocol must be string"))
			} else if proto.Value != "TCP" && proto.Value != "UDP" {
				errs = append(errs, lineErr(file, proto.Line, fmt.Sprintf("protocol has unsupported value '%s'", proto.Value)))
			}
		}
	}

	return errs
}

// ---- Probes ----

func validateProbe(file string, n *yaml.Node, field string) []ValError {
	if n.Kind != yaml.MappingNode {
		return []ValError{lineErr(file, n.Line, fmt.Sprintf("%s must be object", field))}
	}
	httpGet := get(n, "httpGet")
	if httpGet == nil {
		return []ValError{req(file, field+".httpGet")}
	}
	return validateHTTPGet(file, httpGet)
}

func validateHTTPGet(file string, n *yaml.Node) []ValError {
	if n.Kind != yaml.MappingNode {
		return []ValError{lineErr(file, n.Line, "httpGet must be object")}
	}

	var errs []ValError

	path := get(n, "path")
	if path == nil {
		errs = append(errs, req(file, "path"))
	} else if path.Kind != yaml.ScalarNode || path.Tag != "!!str" {
		errs = append(errs, lineErr(file, path.Line, "path must be string"))
	} else if !strings.HasPrefix(path.Value, "/") {
		errs = append(errs, lineErr(file, path.Line, fmt.Sprintf("path has invalid format '%s'", path.Value)))
	}

	port := get(n, "port")
	if port == nil {
		errs = append(errs, req(file, "port"))
	} else {
		errs = append(errs, validatePort(file, port, "port")...)
	}

	return errs
}

// ---- Resources ----

func validateResources(file string, n *yaml.Node) []ValError {
	if n.Kind != yaml.MappingNode {
		return []ValError{lineErr(file, n.Line, "resources must be object")}
	}
	var errs []ValError

	if reqs := get(n, "requests"); reqs != nil {
		errs = append(errs, validateResourceMap(file, reqs)...)
	}
	if lims := get(n, "limits"); lims != nil {
		errs = append(errs, validateResourceMap(file, lims)...)
	}

	return errs
}

func validateResourceMap(file string, n *yaml.Node) []ValError {
	if n.Kind != yaml.MappingNode {
		return []ValError{lineErr(file, n.Line, "resources must be object")}
	}

	var errs []ValError

	if cpu := get(n, "cpu"); cpu != nil {
		if !isIntScalar(cpu) {
			errs = append(errs, lineErr(file, cpu.Line, "cpu must be int"))
		}
	}
	if mem := get(n, "memory"); mem != nil {
		if mem.Kind != yaml.ScalarNode || mem.Tag != "!!str" {
			errs = append(errs, lineErr(file, mem.Line, "memory must be string"))
		} else if !memRe.MatchString(mem.Value) {
			errs = append(errs, lineErr(file, mem.Line, fmt.Sprintf("memory has invalid format '%s'", mem.Value)))
		}
	}

	return errs
}

// ---- Helpers ----

func firstDocMapping(root *yaml.Node) *yaml.Node {
	if root == nil {
		return nil
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		if root.Content[0].Kind == yaml.MappingNode {
			return root.Content[0]
		}
		return nil
	}
	if len(root.Content) > 0 && root.Content[0].Kind == yaml.MappingNode {
		return root.Content[0]
	}
	return nil
}

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(m.Content); i += 2 {
		k := m.Content[i]
		v := m.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			return v
		}
	}
	return nil
}

func req(file, field string) ValError {
	return ValError{File: file, Line: 0, Msg: field + " is required"}
}

func lineErr(file string, line int, msg string) ValError {
	return ValError{File: file, Line: line, Msg: msg}
}

func mustString(file string, n *yaml.Node, field string) []ValError {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return []ValError{lineErr(file, n.Line, fmt.Sprintf("%s must be string", field))}
	}
	return nil
}

func isIntScalar(n *yaml.Node) bool {
	if n.Kind != yaml.ScalarNode {
		return false
	}
	_, err := strconv.ParseInt(strings.TrimSpace(n.Value), 10, 64)
	return err == nil
}

func validatePort(file string, n *yaml.Node, field string) []ValError {
	if n.Kind != yaml.ScalarNode {
		return []ValError{lineErr(file, n.Line, fmt.Sprintf("%s must be int", field))}
	}
	v, err := strconv.Atoi(strings.TrimSpace(n.Value))
	if err != nil {
		return []ValError{lineErr(file, n.Line, fmt.Sprintf("%s must be int", field))}
	}
	if v <= 0 || v >= 65536 {
		return []ValError{lineErr(file, n.Line, fmt.Sprintf("%s value out of range", field))}
	}
	return nil
}

func validImage(s string) bool {
	// must be registry.bigbrother.io/<name>:<tag>
	if !strings.HasPrefix(s, "registry.bigbrother.io/") {
		return false
	}
	lastSlash := strings.LastIndex(s, "/")
	colon := strings.LastIndex(s, ":")
	return colon > lastSlash && colon < len(s)-1
}
