package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	coreapi "k8s.io/api/core/v1"
	rbacapi "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
	rbacclientset "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	imageapi "github.com/openshift/api/image/v1"
	projectapi "github.com/openshift/api/project/v1"
	templateapi "github.com/openshift/api/template/v1"
	imageclientset "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	"github.com/openshift/client-go/project/clientset/versioned"
	templatescheme "github.com/openshift/client-go/template/clientset/versioned/scheme"

	"github.com/openshift/ci-operator/pkg/api"
	"github.com/openshift/ci-operator/pkg/interrupt"
	"github.com/openshift/ci-operator/pkg/steps"
)

const usage = `Orchestrate multi-stage image-based builds

The ci-operator reads a declarative configuration JSON file and executes a set of build
steps on an OpenShift cluster for image-based components. By default, all steps are run,
but a caller may select one or more targets (image names or test names) to limit to only
steps that those targets depend on. The build creates a new project to run the builds in
and can automatically clean up the project when the build completes.

ci-operator leverages declarative OpenShift builds and images to reuse previously compiled
artifacts. It makes building multiple images that share one or more common base layers
simple as well as running tests that depend on those images.

Since the command is intended for use in CI environments it requires an input environment
variable called the JOB_SPEC that defines the GitHub project to execute and the commit,
branch, and any PRs to merge onto the branch. See the kubernetes/test-infra project for
a description of JOB_SPEC.

The inputs of the build (source code, tagged images, configuration) are combined to form
a consistent name for the target namespace that will change if any of the inputs change.
This allows multiple test jobs to share common artifacts and still perform retries.

The standard build steps are designed for simple command-line actions (like invoking
"make test") but can be extended by passing one or more templates via the --template flag.
The name of the template defines the stage and the template must contain at least one
pod. The parameters passed to the template are the current process environment and a set
of dynamic parameters that are inferred from previous steps. These parameters are:

  NAMESPACE
    The namespace generated by the operator for the given inputs or the value of
    --namespace.

  IMAGE_FORMAT
    A string that points to the public image repository URL of the image stream(s)
    created by the tag step. Example:

      registry.svc.ci.openshift.org/ci-op-9o8bacu/stable:${component}

    Will cause the template to depend on all image builds.

  IMAGE_<component>
    The public image repository URL for an output image. If specified the template
    will depend on the image being built.

  JOB_NAME
    The job name from the JOB_SPEC

  JOB_NAME_SAFE
    The job name in a form safe for use as a Kubernetes resource name.

  JOB_NAME_HASH
    A short hash of the job name for making tasks unique.

  RPM_REPO
    If the job creates RPMs this will be the public URL that can be used as the
    baseurl= value of an RPM repository.

Dynamic environment variables are overriden by process environment variables.

Both test and template jobs can gather artifacts created by pods. Set
--artifact-dir to define the top level artifact directory, and any test task
that defines artifact_dir or template that has an "artifacts" volume mounted
into a container will have artifacts extracted after the container has completed.
Errors in artifact extraction will not cause build failures.

`

func main() {
	flagSet := flag.NewFlagSet("", flag.ExitOnError)
	opt := bindOptions(flagSet)
	flagSet.Parse(os.Args[1:])
	if opt.verbose {
		flag.CommandLine.Set("alsologtostderr", "true")
		flag.CommandLine.Set("v", "10")
	}
	if opt.help {
		fmt.Printf(usage)
		flagSet.Usage()
		os.Exit(0)
	}

	if err := opt.Validate(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}

	if err := opt.Complete(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}

	if err := opt.Run(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}

type stringSlice struct {
	values []string
}

func (s *stringSlice) String() string {
	return strings.Join(s.values, string(filepath.Separator))
}

func (s *stringSlice) Set(value string) error {
	s.values = append(s.values, value)
	return nil
}

type options struct {
	configSpecPath    string
	templatePaths     stringSlice
	secretDirectories stringSlice

	target string

	verbose bool
	help    bool
	dry     bool

	writeParams string
	artifactDir string

	namespace           string
	baseNamespace       string
	idleCleanupDuration time.Duration

	inputHash     string
	secrets       []*coreapi.Secret
	templates     []*templateapi.Template
	configSpec    *api.ReleaseBuildConfiguration
	jobSpec       *steps.JobSpec
	clusterConfig *rest.Config
}

func bindOptions(flag *flag.FlagSet) *options {
	opt := &options{}

	// command specific options
	flag.BoolVar(&opt.help, "h", false, "See help for this command.")
	flag.BoolVar(&opt.verbose, "v", false, "Show verbose output.")

	// what we will run
	flag.StringVar(&opt.configSpecPath, "config", "", "The configuration file. If not specified the CONFIG_SPEC environment variable will be used.")
	flag.StringVar(&opt.target, "target", "", "A config nofig to target. Only steps that are required for this target will be run.")
	flag.BoolVar(&opt.dry, "dry-run", true, "Do not contact the API server.")

	// add to the graph of things we run or create
	flag.Var(&opt.templatePaths, "template", "A set of paths to optional templates to add as stages to this job. Each template is expected to contain at least one restart=Never pod. Parameters are filled from environment or from the automatic parameters generated by the operator.")
	flag.Var(&opt.secretDirectories, "secret-dir", "One or more directories that should converted into secrets in the test namespace.")

	// the target namespace and cleanup behavior
	flag.StringVar(&opt.namespace, "namespace", "", "Namespace to create builds into, defaults to build_id from JOB_SPEC. If the string '{id}' is in this value it will be replaced with the build input hash.")
	flag.StringVar(&opt.baseNamespace, "base-namespace", "stable", "Namespace to read builds from, defaults to stable.")
	flag.DurationVar(&opt.idleCleanupDuration, "delete-when-idle", opt.idleCleanupDuration, "If no pod is running for longer than this interval, delete the namespace.")

	// output control
	flag.StringVar(&opt.artifactDir, "artifact-dir", "", "If set grab artifacts from test and template jobs.")
	flag.StringVar(&opt.writeParams, "write-params", "", "If set write an env-compatible file with the output of the job.")

	return opt
}

func (o *options) Validate() error {
	return nil
}

func (o *options) Complete() error {
	var configSpec string
	if len(o.configSpecPath) > 0 {
		data, err := ioutil.ReadFile(o.configSpecPath)
		if err != nil {
			return fmt.Errorf("--config error: %v", err)
		}
		configSpec = string(data)
	} else {
		var ok bool
		configSpec, ok = os.LookupEnv("CONFIG_SPEC")
		if !ok || len(configSpec) == 0 {
			return fmt.Errorf("CONFIG_SPEC environment variable is not set or empty and no --config file was set")
		}
	}
	if err := json.Unmarshal([]byte(configSpec), &o.configSpec); err != nil {
		return fmt.Errorf("invalid configuration: %v\nvalue:\n%s", err, string(configSpec))
	}

	jobSpec, err := steps.ResolveSpecFromEnv()
	if err != nil {
		return fmt.Errorf("failed to resolve job spec: %v", err)
	}
	jobSpec.SetBaseNamespace(o.baseNamespace)
	o.jobSpec = jobSpec

	for _, path := range o.secretDirectories.values {
		secret := &coreapi.Secret{Data: make(map[string][]byte)}
		secret.Type = coreapi.SecretTypeOpaque
		secret.Name = filepath.Base(path)
		files, err := ioutil.ReadDir(path)
		if err != nil {
			return err
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			path := filepath.Join(path, f.Name())
			// if the file is a broken symlink or a symlink to a dir, skip it
			if fi, err := os.Stat(path); err != nil || fi.IsDir() {
				continue
			}
			secret.Data[f.Name()], err = ioutil.ReadFile(path)
			if err != nil {
				return err
			}
		}
		o.secrets = append(o.secrets, secret)
	}

	for _, path := range o.templatePaths.values {
		contents, err := ioutil.ReadFile(path)
		if err != nil {
			return err
		}
		obj, gvk, err := templatescheme.Codecs.UniversalDeserializer().Decode(contents, nil, nil)
		if err != nil {
			return fmt.Errorf("unable to parse template %s: %v", path, err)
		}
		template, ok := obj.(*templateapi.Template)
		if !ok {
			return fmt.Errorf("%s is not a template: %v", path, gvk)
		}
		if len(template.Name) == 0 {
			template.Name = filepath.Base(path)
			template.Name = strings.TrimSuffix(template.Name, filepath.Ext(template.Name))
		}
		o.templates = append(o.templates, template)
	}

	clusterConfig, err := loadClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to load cluster config: %v", err)
	}
	o.clusterConfig = clusterConfig

	return nil
}

func (o *options) Run() error {
	start := time.Now()
	defer func() {
		log.Printf("Ran for %s", time.Now().Sub(start).Truncate(time.Second))
	}()

	// load the graph from the configuration
	buildSteps, err := steps.FromConfig(o.configSpec, o.jobSpec, o.templates, o.writeParams, o.artifactDir, o.clusterConfig)
	if err != nil {
		return fmt.Errorf("failed to generate steps from config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	handler := func(s os.Signal) {
		if o.dry {
			os.Exit(0)
		}
		log.Printf("error: Process interrupted with signal %s, exiting in 2s ...", s)
		cancel()
		time.Sleep(2 * time.Second)
		os.Exit(1)
	}

	return interrupt.New(handler).Run(func() error {
		// before we create the namespace, we need to ensure all inputs to the graph
		// have been resolved
		if err := o.resolveInputs(ctx, buildSteps); err != nil {
			return err
		}

		// convert the full graph into the subset we must run
		nodes, err := api.BuildPartialGraph(buildSteps, []string{o.target})
		if err != nil {
			return err
		}

		// initialize the namespace if necessary and create any resources that must
		// exist prior to execution
		if err := o.initializeNamespace(); err != nil {
			return err
		}

		// execute the graph
		return steps.Run(ctx, nodes, o.dry)
	})
}

// loadClusterConfig loads connection configuration
// for the cluster we're deploying to. We prefer to
// use in-cluster configuration if possible, but will
// fall back to using default rules otherwise.
func loadClusterConfig() (*rest.Config, error) {
	clusterConfig, err := rest.InClusterConfig()
	if err == nil {
		return clusterConfig, nil
	}

	credentials, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		return nil, fmt.Errorf("could not load credentials from config: %v", err)
	}

	clusterConfig, err = clientcmd.NewDefaultClientConfig(*credentials, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("could not load client configuration: %v", err)
	}
	return clusterConfig, nil
}

func (o *options) resolveInputs(ctx context.Context, steps []api.Step) error {
	var inputs api.InputDefinition
	for _, step := range steps {
		definition, err := step.Inputs(ctx, o.dry)
		if err != nil {
			return err
		}
		inputs = append(inputs, definition...)
	}

	// a change in the config for the build changes the output
	configSpec, err := json.Marshal(o.configSpec)
	if err != nil {
		panic(err)
	}
	inputs = append(inputs, string(configSpec))

	o.inputHash = inputHash(inputs)

	// input hash is unique for a given job definition and input refs
	if len(o.namespace) == 0 {
		o.namespace = "ci-op-{id}"
	}
	o.namespace = strings.Replace(o.namespace, "{id}", o.inputHash, -1)
	// TODO: instead of mutating this here, we should pass the parts of graph execution that are resolved
	// after the graph is created but before it is run down into the run step.
	o.jobSpec.SetNamespace(o.namespace)

	return nil
}

func (o *options) initializeNamespace() error {
	if o.dry {
		return nil
	}
	projectGetter, err := versioned.NewForConfig(o.clusterConfig)
	if err != nil {
		return fmt.Errorf("could not get project client for cluster config: %v", err)
	}

	log.Printf("Creating namespace %s", o.namespace)
	for {
		project, err := projectGetter.ProjectV1().ProjectRequests().Create(&projectapi.ProjectRequest{
			ObjectMeta: meta.ObjectMeta{
				Name: o.namespace,
			},
			DisplayName: fmt.Sprintf("%s - %s", o.namespace, o.jobSpec.Job),
			Description: jobDescription(o.jobSpec, o.configSpec),
		})
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("could not set up namespace for test: %v", err)
		}
		if err != nil {
			project, err = projectGetter.ProjectV1().Projects().Get(o.namespace, meta.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("cannot retrieve test namespace: %v", err)
			}
		}
		if project.Status.Phase == coreapi.NamespaceTerminating {
			log.Println("Waiting for namespace to finish terminating before creating another")
			time.Sleep(3 * time.Second)
			continue
		}
		break
	}

	if o.idleCleanupDuration > 0 {
		if err := o.createNamespaceCleanupPod(); err != nil {
			return err
		}
	}

	imageGetter, err := imageclientset.NewForConfig(o.clusterConfig)
	if err != nil {
		return fmt.Errorf("could not get image client for cluster config: %v", err)
	}

	// create the image stream or read it to get its uid
	is, err := imageGetter.ImageStreams(o.jobSpec.Namespace()).Create(&imageapi.ImageStream{
		ObjectMeta: meta.ObjectMeta{
			Namespace: o.jobSpec.Namespace(),
			Name:      steps.PipelineImageStream,
		},
		Spec: imageapi.ImageStreamSpec{
			// pipeline:* will now be directly referenceable
			LookupPolicy: imageapi.ImageLookupPolicy{Local: true},
		},
	})
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("could not set up pipeline imagestream for test: %v", err)
		}
		is, _ = imageGetter.ImageStreams(o.jobSpec.Namespace()).Get(steps.PipelineImageStream, meta.GetOptions{})
	}
	if is != nil {
		isTrue := true
		o.jobSpec.SetOwner(&meta.OwnerReference{
			APIVersion: "image.openshift.io/v1",
			Kind:       "ImageStream",
			Name:       steps.PipelineImageStream,
			UID:        is.UID,
			Controller: &isTrue,
		})
	}

	client, err := coreclientset.NewForConfig(o.clusterConfig)
	if err != nil {
		return fmt.Errorf("could not get core client for cluster config: %v", err)
	}
	for _, secret := range o.secrets {
		_, err := client.Secrets(o.namespace).Create(secret)
		if errors.IsAlreadyExists(err) {
			existing, err := client.Secrets(o.namespace).Get(secret.Name, meta.GetOptions{})
			if err != nil {
				return err
			}
			for k, v := range secret.Data {
				existing.Data[k] = v
			}
			if _, err := client.Secrets(o.namespace).Update(existing); err != nil {
				return err
			}
			log.Printf("Updated secret %s", secret.Name)
			continue
		}
		if err != nil {
			return err
		}
		log.Printf("Created secret %s", secret.Name)
	}
	return nil
}

// createNamespaceCleanupPod creates a pod that deletes the job namespace if no other run-once pods are running
// for more than idleCleanupDuration.
func (o *options) createNamespaceCleanupPod() error {
	log.Printf("Namespace will be deleted after %s of idle time", o.idleCleanupDuration)
	client, err := coreclientset.NewForConfig(o.clusterConfig)
	if err != nil {
		return fmt.Errorf("could not get image client for cluster config: %v", err)
	}
	rbacClient, err := rbacclientset.NewForConfig(o.clusterConfig)
	if err != nil {
		return fmt.Errorf("could not get image client for cluster config: %v", err)
	}

	if _, err := client.ServiceAccounts(o.namespace).Create(&coreapi.ServiceAccount{
		ObjectMeta: meta.ObjectMeta{
			Name: "cleanup",
		},
	}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("could not create service account for cleanup: %v", err)
	}
	if _, err := rbacClient.RoleBindings(o.namespace).Create(&rbacapi.RoleBinding{
		ObjectMeta: meta.ObjectMeta{
			Name: "cleanup",
		},
		Subjects: []rbacapi.Subject{{Kind: "ServiceAccount", Name: "cleanup"}},
		RoleRef: rbacapi.RoleRef{
			Kind: "ClusterRole",
			Name: "admin",
		},
	}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("could not create role binding for cleanup: %v", err)
	}

	grace := int64(30)
	deadline := int64(12 * time.Hour / time.Second)
	if _, err := client.Pods(o.namespace).Create(&coreapi.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "cleanup-when-idle",
		},
		Spec: coreapi.PodSpec{
			ActiveDeadlineSeconds:         &deadline,
			RestartPolicy:                 coreapi.RestartPolicyNever,
			TerminationGracePeriodSeconds: &grace,
			ServiceAccountName:            "cleanup",
			Containers: []coreapi.Container{
				{
					Name:  "cleanup",
					Image: "openshift/origin-cli:latest",
					Env: []coreapi.EnvVar{
						{
							Name:      "NAMESPACE",
							ValueFrom: &coreapi.EnvVarSource{FieldRef: &coreapi.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
						},
						{
							Name:  "WAIT",
							Value: fmt.Sprintf("%d", int(o.idleCleanupDuration.Seconds())),
						},
					},
					Command: []string{"/bin/bash", "-c"},
					Args: []string{`
						#!/bin/bash
						set -euo pipefail

						function cleanup() {
							set +e
							oc delete project ${NAMESPACE}
						}

						trap 'kill $(jobs -p); echo "Pod deleted, deleting project ..."; exit 1' TERM
						trap cleanup EXIT

						echo "Waiting for all running pods to terminate (max idle ${WAIT}s) ..."
						count=0
						while true; do
							alive="$( oc get pods --template '{{ range .items }}{{ if and (not (eq .metadata.name "cleanup-when-idle")) (eq .spec.restartPolicy "Never") (or (eq .status.phase "Pending") (eq .status.phase "Running") (eq .status.phase "Unknown")) }} {{ .metadata.name }}{{ end }}{{ end }}' )"
							if [[ -n "${alive}" ]]; then
								count=0
								sleep ${WAIT} & wait
								continue
							fi
							if [[ "${count}" -lt 1 ]]; then
								count+=1
								sleep ${WAIT} & wait
								continue
							fi
							echo "No pods running for more than ${WAIT}s, deleting project ..."
							exit 0
						done
						`,
					},
				},
			},
		},
	}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("could not create pod for cleanup: %v", err)
	}
	return nil
}

// inputHash returns a string that hashes the unique parts of the input to avoid collisions.
func inputHash(inputs api.InputDefinition) string {
	hash := sha256.New()

	// the inputs form a part of the hash
	for _, s := range inputs {
		hash.Write([]byte(s))
	}

	// Object names can't be too long so we truncate
	// the hash. This increases chances of collision
	// but we can tolerate it as our input space is
	// tiny.
	return fmt.Sprintf("%x", hash.Sum(nil))[54:]
}

// jobDescription returns a string representing the job's description.
func jobDescription(job *steps.JobSpec, config *api.ReleaseBuildConfiguration) string {
	var links []string
	for _, pull := range job.Refs.Pulls {
		links = append(links, fmt.Sprintf("https://github.com/%s/%s/pull/%d - %s", job.Refs.Org, job.Refs.Repo, pull.Number, pull.Author))
	}
	if len(links) > 0 {
		return fmt.Sprintf("%s on https://github.com/%s/%s\n\n%s", job.Job, job.Refs.Org, job.Refs.Repo, strings.Join(links, "\n"))
	}
	return fmt.Sprintf("%s on https://github.com/%s/%s ref=%s commit=%s", job.Job, job.Refs.Org, job.Refs.Repo, job.Refs.BaseRef, job.Refs.BaseSHA)
}