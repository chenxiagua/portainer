package exec

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/http/proxy"
	"github.com/portainer/portainer/api/http/proxy/factory"
	"github.com/portainer/portainer/api/http/proxy/factory/kubernetes"
	"github.com/portainer/portainer/api/kubernetes/cli"

	"github.com/pkg/errors"
)

// KubernetesDeployer represents a service to deploy resources inside a Kubernetes environment(endpoint).
type KubernetesDeployer struct {
	binaryPath                  string
	dataStore                   dataservices.DataStore
	reverseTunnelService        portainer.ReverseTunnelService
	signatureService            portainer.DigitalSignatureService
	kubernetesClientFactory     *cli.ClientFactory
	kubernetesTokenCacheManager *kubernetes.TokenCacheManager
	proxyManager                *proxy.Manager
}

// NewKubernetesDeployer initializes a new KubernetesDeployer service.
func NewKubernetesDeployer(kubernetesTokenCacheManager *kubernetes.TokenCacheManager, kubernetesClientFactory *cli.ClientFactory, datastore dataservices.DataStore, reverseTunnelService portainer.ReverseTunnelService, signatureService portainer.DigitalSignatureService, proxyManager *proxy.Manager, binaryPath string) *KubernetesDeployer {
	return &KubernetesDeployer{
		binaryPath:                  binaryPath,
		dataStore:                   datastore,
		reverseTunnelService:        reverseTunnelService,
		signatureService:            signatureService,
		kubernetesClientFactory:     kubernetesClientFactory,
		kubernetesTokenCacheManager: kubernetesTokenCacheManager,
		proxyManager:                proxyManager,
	}
}

func (deployer *KubernetesDeployer) getToken(userID portainer.UserID, endpoint *portainer.Endpoint, setLocalAdminToken bool) (string, error) {
	kubeCLI, err := deployer.kubernetesClientFactory.GetPrivilegedKubeClient(endpoint)
	if err != nil {
		return "", err
	}

	tokenCache := deployer.kubernetesTokenCacheManager.GetOrCreateTokenCache(endpoint.ID)

	tokenManager, err := kubernetes.NewTokenManager(kubeCLI, deployer.dataStore, tokenCache, setLocalAdminToken)
	if err != nil {
		return "", err
	}

	user, err := deployer.dataStore.User().Read(userID)
	if err != nil {
		return "", errors.Wrap(err, "failed to fetch the user")
	}

	if user.Role == portainer.AdministratorRole {
		return tokenManager.GetAdminServiceAccountToken(), nil
	}

	token, err := tokenManager.GetUserServiceAccountToken(int(user.ID), endpoint.ID)
	if err != nil {
		return "", err
	}

	if token == "" {
		return "", errors.New("can not get a valid user service account token")
	}

	return token, nil
}

// Deploy upserts Kubernetes resources defined in manifest(s)
func (deployer *KubernetesDeployer) Deploy(userID portainer.UserID, endpoint *portainer.Endpoint, manifestFiles []string, namespace string) (string, error) {
	return deployer.command("apply", userID, endpoint, manifestFiles, namespace)
}

// Remove deletes Kubernetes resources defined in manifest(s)
func (deployer *KubernetesDeployer) Remove(userID portainer.UserID, endpoint *portainer.Endpoint, manifestFiles []string, namespace string) (string, error) {
	return deployer.command("delete", userID, endpoint, manifestFiles, namespace)
}

func (deployer *KubernetesDeployer) command(operation string, userID portainer.UserID, endpoint *portainer.Endpoint, manifestFiles []string, namespace string) (string, error) {
	token, err := deployer.getToken(userID, endpoint, endpoint.Type == portainer.KubernetesLocalEnvironment)
	if err != nil {
		return "", errors.Wrap(err, "failed generating a user token")
	}

	command := path.Join(deployer.binaryPath, "kubectl")
	if runtime.GOOS == "windows" {
		command = path.Join(deployer.binaryPath, "kubectl.exe")
	}

	args := []string{"--token", token}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}

	if endpoint.Type == portainer.AgentOnKubernetesEnvironment || endpoint.Type == portainer.EdgeAgentOnKubernetesEnvironment {
		url, proxy, err := deployer.getAgentURL(endpoint)
		if err != nil {
			return "", errors.WithMessage(err, "failed generating endpoint URL")
		}

		defer proxy.Close()
		args = append(args, "--server", url)
		args = append(args, "--insecure-skip-tls-verify")
	}

	if operation == "delete" {
		args = append(args, "--ignore-not-found=true")
	}

	args = append(args, operation)
	for _, path := range manifestFiles {
		args = append(args, "-f", strings.TrimSpace(path))
	}

	var stderr bytes.Buffer
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "POD_NAMESPACE=default")
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		return "", errors.Wrapf(err, "failed to execute kubectl command: %q", stderr.String())
	}

	return string(output), nil
}

func (deployer *KubernetesDeployer) getAgentURL(endpoint *portainer.Endpoint) (string, *factory.ProxyServer, error) {
	proxy, err := deployer.proxyManager.CreateAgentProxyServer(endpoint)
	if err != nil {
		return "", nil, err
	}

	return fmt.Sprintf("http://127.0.0.1:%d/kubernetes", proxy.Port), proxy, nil
}
