package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/fernferret/envy"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	//
	// Uncomment to load all auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
	//
	// Or uncomment to load specific auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s [deployment/]{name}:\n", os.Args[0])
	pflag.PrintDefaults()
}

func parseImageSource(ctx context.Context, name string) (types.ImageSource, error) {
	ref, err := alltransports.ParseImageName(name)
	if err != nil {
		return nil, err
	}
	sys := &types.SystemContext{}
	return ref.NewImageSource(ctx, sys)
}

type ImageInfo struct {
	Cmd        []string
	Entrypoint []string
	WorkingDir string
}

func inspectImage(imageName string) (*ImageInfo, error) {
	ctx := context.Background()
	sys := &types.SystemContext{}
	src, err := parseImageSource(ctx, imageName)
	if err != nil {
		return nil, fmt.Errorf("Error parsing image source: %w", err)
	}
	img, err := image.FromUnparsedImage(ctx, sys, image.UnparsedInstance(src, nil))
	if err != nil {
		return nil, fmt.Errorf("Error parsing manifest for image: %w", err)
	}
	myImg, err := img.OCIConfig(ctx)
	// imgInspect, err := img.Inspect(ctx)
	if err != nil {
		return nil, fmt.Errorf("Error inspecting image: %w", err)
	}
	return &ImageInfo{
		WorkingDir: myImg.Config.WorkingDir,
		Entrypoint: myImg.Config.Entrypoint,
		Cmd:        myImg.Config.Cmd,
	}, nil
}

func loadCurrentNamespace(kubeconfig string) (string, error) {
	kubectlconfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{
			CurrentContext: "",
		}).RawConfig()
	if err != nil {
		return "", err
	}
	currentContext := kubectlconfig.CurrentContext
	ctx, ok := kubectlconfig.Contexts[currentContext]
	if !ok {
		return "", fmt.Errorf("current context %q from kubeconfig %q not found, this is a misconfiguration on your part", currentContext, kubeconfig)
	}
	return ctx.Namespace, nil
}

func main() {
	pflag.Usage = usage
	var kubeconfig, namespace, skopeoTransport string
	var force bool
	pflag.StringVarP(&namespace, "namespace", "n", "", "If present, the `namespace` scope for this CLI request")
	pflag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file, KUBECONFIG will be used if absent")
	pflag.BoolVarP(&force, "force", "f", false, "remove an old devpod if it existed")
	pflag.StringVar(&skopeoTransport, "skopeo-transport", "docker://", "set the transport to use when looking up remote container information")
	// nameTemplate := pflag.String("name", "%s-devpod", "Set a name template to create the new resource")

	envy.SetEnvName("kubeconfig", "KUBECONFIG")
	envy.Parse("DEVPOD")
	pflag.Parse()

	if len(pflag.Args()) < 1 {
		fmt.Fprintf(os.Stderr, "ERROR: missing 'name' argument, see --help.\n")
		os.Exit(1)
	}

	// Load the kubeconfig first from the command line, then from KUBECONFIG (via
	// envy). If we still don't have one, try to set it from the homedir.
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	if namespace == "" {
		namespace, err = loadCurrentNamespace(kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	}

	name := pflag.Arg(0)
	resource := "pod"
	if strings.Contains(name, "/") {
		splitList := strings.SplitN(name, "/", 2)
		resource = strings.ToLower(splitList[0])
		name = splitList[1]
	}

	switch resource {
	// case "pod", "pods", "po":
	case "deployment", "deployments", "deploy", "dp":
		createDeployment(clientset, name, "deployment", namespace, skopeoTransport, force)
	// case "statefulset", "statefulsets", "sts":
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unrecognized resource type: %q, see --help for info. Only standard kubernetes types are supported.\n", resource)
		os.Exit(1)
	}
}

func createInitContainer(pod *v1.PodSpec, resource, namespace, name, skopeoTransport string) *v1.ConfigMap {
	cm := v1.ConfigMap{}
	cm.Name = fmt.Sprintf("%s-devpod-init", name)
	cm.Namespace = namespace
	cm.Data = map[string]string{}
	for idx, item := range pod.Containers {
		imageDetails, _ := inspectImage(fmt.Sprintf("%s%s", skopeoTransport, item.Image))
		containerName := item.Name
		filename := fmt.Sprintf("%d_%s.sh", idx, containerName)
		script := "#!/bin/sh\n\n"
		if item.WorkingDir != "" {
			script = fmt.Sprintf("%secho 'Setting WorkingDir via: cd %s';\n", script, item.WorkingDir)
			script = fmt.Sprintf("%scd %s;\n\n", script, item.WorkingDir)
		}
		var savedArgs, savedCmd []string

		var lineInScript []string

		if len(item.Command) > 0 {
			copy(savedCmd, item.Command)
			script = fmt.Sprintf("%s# Command (ENTRYPOINT) from container:\n# %s\n", script, strings.Join(item.Command, " "))
			lineInScript = item.Command
		}
		if len(imageDetails.Entrypoint) > 0 {
			script = fmt.Sprintf("%s# Command (ENTRYPOINT) from image:\n# %s\n", script, strings.Join(imageDetails.Entrypoint, " "))
			if len(item.Command) == 0 {
				lineInScript = imageDetails.Entrypoint
			}
		}

		if len(item.Args) > 0 {
			copy(savedArgs, item.Args)
			script = fmt.Sprintf("%s# Args (CMD) from container:\n# %s\n", script, strings.Join(item.Args, " "))
			lineInScript = append(lineInScript, item.Args...)
		}
		if len(imageDetails.Cmd) > 0 {
			script = fmt.Sprintf("%s# Args (CMD) from image:\n# %s\n", script, strings.Join(imageDetails.Cmd, " "))
			if len(item.Args) == 0 {
				lineInScript = append(lineInScript, imageDetails.Cmd...)
			}
		}
		script = fmt.Sprintf("%s\n%s\n", script, strings.Join(lineInScript, " "))

		cm.Data[filename] = script
		item.Command = []string{
			"sh",
			"-c",
		}
		item.Args = []string{
			fmt.Sprintf(`echo "Welcome to DEVPOD"
echo "This is a copy of the %s %s/%s"
echo "All it does is just sleep forever and ever"
echo ""
echo "The existing entrypoint was combined and placed: TODO"

sleep infinity`, resource, namespace, name),
		}
		pod.Containers[idx] = item
	}

	return &cm
}

func createDeployment(clientset *kubernetes.Clientset, name, resource, namespace, skopeoTransport string, force bool) {
	dp, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to find %s %q in namespace %q, cannot create devpod: %s\n", resource, name, namespace, err)
		os.Exit(1)
	}

	// Check for an existing devpod to at least get its UID
	newName := fmt.Sprintf("%s-devpod", dp.Name)
	newDp, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), newName, metav1.GetOptions{})
	if err != nil {
		if !k8serr.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "Unable to search for %s %q in namespace %q, cannot create devpod: %s\n", resource, name, namespace, err)
			os.Exit(1)
		}
		dp.UID = ""
		newDp = nil
	} else {
		fmt.Println(dp.UID)
		fmt.Println(newDp.UID)
		dp.UID = newDp.UID
	}
	dp.Name = newName
	// Reset the resource version for new objects.
	dp.ResourceVersion = ""

	// Rename at least one key so this pod doesn't match the production version
	keys := make([]string, 0, len(dp.Spec.Selector.MatchLabels))
	for key := range dp.Spec.Selector.MatchLabels {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	// There must be at least one label selector
	savedVal := dp.Spec.Selector.MatchLabels[keys[0]]
	dp.Spec.Selector.MatchLabels[keys[0]] = fmt.Sprintf("%s-devpod", savedVal)
	dp.Spec.Template.Labels[keys[0]] = fmt.Sprintf("%s-devpod", savedVal)

	// Always move back to 1 replica
	replicas := int32(1)
	dp.Spec.Replicas = &replicas

	if dp.Spec.Template.Labels == nil {
		dp.Spec.Template.Labels = map[string]string{}
	}
	if dp.Spec.Template.Annotations == nil {
		dp.Spec.Template.Annotations = map[string]string{}
	}

	dp.Spec.Template.Labels["devpod"] = "devpod"
	dp.Spec.Template.Annotations["devpod"] = "Created by devpod"
	dp.Spec.Selector.MatchLabels["devpod"] = "devpod"
	termGracePeriod := int64(1)
	dp.Spec.Template.Spec.TerminationGracePeriodSeconds = &termGracePeriod

	// dp.Spec.Template.Spec
	cm := createInitContainer(&dp.Spec.Template.Spec, resource, namespace, name, skopeoTransport)
	existingCm, err := clientset.CoreV1().ConfigMaps(cm.Namespace).Get(context.TODO(), cm.Name, metav1.GetOptions{})
	if err != nil {
		if !k8serr.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to check for configmap %q in namespace %q: %s\n", cm.Name, cm.Namespace, err)
			os.Exit(1)
		} else {
			// Need to create
			_, err := clientset.CoreV1().ConfigMaps(cm.Namespace).Create(context.TODO(), cm, metav1.CreateOptions{})
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to create configmap %q in namespace %q: %s\n", cm.Name, cm.Namespace, err)
				os.Exit(1)
			}
		}
	} else {
		// Need to update
		cm.UID = existingCm.UID
		_, err := clientset.CoreV1().ConfigMaps(cm.Namespace).Update(context.TODO(), cm, metav1.UpdateOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to update configmap %q in namespace %q: %s\n", cm.Name, cm.Namespace, err)
			os.Exit(1)
		}
	}

	var createdDp *appsv1.Deployment
	var verb string
	if newDp == nil {
		verb = "create"
		createdDp, err = clientset.AppsV1().Deployments(namespace).Create(context.TODO(), dp, metav1.CreateOptions{})
	} else {
		verb = "update"
		createdDp, err = clientset.AppsV1().Deployments(namespace).Update(context.TODO(), dp, metav1.UpdateOptions{})
	}
	if err != nil {
		if force {
			dp.UID = ""
			fmt.Printf("Devpod %s/%s already exists, removing and re-creating since --force was set.\n", namespace, dp.Name)
			err := clientset.AppsV1().Deployments(namespace).Delete(context.TODO(), dp.Name, metav1.DeleteOptions{})
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to delete and re-create devpod named %q in namespace %q: %s\n", dp.Name, namespace, err)
				os.Exit(1)
			}
			createdDp, err = clientset.AppsV1().Deployments(namespace).Create(context.TODO(), dp, metav1.CreateOptions{})
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to re-create devpod named %q in namespace %q: %s\n", dp.Name, namespace, err)
				os.Exit(1)
			}
		} else {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to %s devpod %q in namespace %q: %s\n", verb, dp.Name, namespace, err)
			fmt.Fprintf(os.Stderr, "You can use --force to delete it and re-create\n")
			os.Exit(1)
		}
	}
	fmt.Fprintf(os.Stdout, "SUCCESS: Created %s/%s, to access run:\n", namespace, createdDp.Name)
	fmt.Fprintf(os.Stdout, " kubectl exec -it -n %q deployment/%q -- sh\n", namespace, createdDp.Name)
}
