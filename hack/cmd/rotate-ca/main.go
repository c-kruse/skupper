package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"time"

	"github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/utils"
	"github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned/typed/skupper/v2alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
)

type RotationContext struct {
	Site *v2alpha1.Site
}

func main() {
	ctx := context.TODO()
	cli, err := client.NewClient("", "", "")
	if err != nil {
		log.Fatal(err)
	}
	skupperCli := cli.GetSkupperClient().SkupperV2alpha1()
	secrets := cli.GetKubeClient().CoreV1().Secrets(cli.Namespace)
	site, err := GetActiveSite(ctx, cli.Namespace, skupperCli)
	if err != nil {
		log.Fatal(err)
	}

	siteID := string(site.ObjectMeta.UID)

	var peerSites []v2alpha1.SiteRecord
	for _, s := range site.Status.Network {
		if s.Id == siteID {
			continue
		}
		for _, link := range s.Links {
			if link.RemoteSiteId == siteID {
				peerSites = append(peerSites, s)
				break
			}
		}

	}
	if len(peerSites) == 0 {
		log.Fatal("site has no incoming links - no need for coordinated rotation process")
	}
	var peerSiteIDs []string
	for _, peerSite := range peerSites {
		peerSiteIDs = append(peerSiteIDs, peerSite.Id)
	}
	prefix := getPrefix()
	labels := map[string]string{
		"internal.skupper.io/hack-rotate-prefix": prefix,
	}
	log.Printf("Beginning certificate rotation for site %s: %s", site.ObjectMeta.Name, siteID)
	log.Printf("using prefix %s", prefix)
	log.Printf("using peer sites %v", peerSiteIDs)
	certdir := "certs-" + prefix
	if err := os.Mkdir(certdir, 0755); err != nil {
		log.Fatal(err)
	}
	ca, err := skupperCli.Certificates(cli.Namespace).Get(ctx, "skupper-site-ca", v1.GetOptions{})
	if err != nil {
		log.Fatal(err)
	}

	next := ca.DeepCopy()
	nextCAName := prefix + "-" + ca.Name
	next.Status = v2alpha1.CertificateStatus{}
	next.ObjectMeta = v1.ObjectMeta{
		Name:   nextCAName,
		Labels: labels,
	}
	log.Printf("creating next CA Certificate %s", nextCAName)
	_, err = skupperCli.Certificates(cli.Namespace).Create(ctx, next, v1.CreateOptions{})
	if err != nil {
		log.Fatal(err)
	}
	utils.RetryErrorWithContext(ctx, time.Millisecond*500, func() error {
		_, err := secrets.Get(ctx, nextCAName, v1.GetOptions{})
		return err
	})
	nextCASecret, err := secrets.Get(ctx, nextCAName, v1.GetOptions{})
	if err != nil {
		log.Fatal(err)
	}
	currCASecret, err := secrets.Get(ctx, ca.Name, v1.GetOptions{})
	if err != nil {
		log.Fatal(err)
	}
	var trustBundle bytes.Buffer
	trustBundle.Write(currCASecret.Data["tls.crt"])
	trustBundle.Write(nextCASecret.Data["tls.crt"])
	log.Printf("updating skupper-site-server trust bundle")
	serverSecret, err := secrets.Get(ctx, "skupper-site-server", v1.GetOptions{})
	if err != nil {
		log.Fatal(err)
	}
	serverSecret.Data["ca.crt"] = trustBundle.Bytes()
	if _, err := secrets.Update(ctx, serverSecret, v1.UpdateOptions{}); err != nil {
		log.Fatal(err)
	}
	siteServerCert, err := skupperCli.Certificates(cli.Namespace).Get(ctx, "skupper-site-server", v1.GetOptions{})
	if err != nil {
		log.Fatal(err)
	}
	nextServerCert := siteServerCert.DeepCopy()
	nextServerName := prefix + "-" + siteServerCert.Name
	nextServerCert.Status = v2alpha1.CertificateStatus{}
	nextServerCert.ObjectMeta = v1.ObjectMeta{
		Name:   nextServerName,
		Labels: labels,
	}
	nextServerCert.Spec.Ca = nextCAName
	log.Printf("creating next skupper-site-server Certificate %s", nextServerName)
	_, err = skupperCli.Certificates(cli.Namespace).Create(ctx, nextServerCert, v1.CreateOptions{})
	if err != nil {
		log.Fatal(err)
	}
	for _, peer := range peerSites {
		name := prefix + "-client-" + peer.Id
		cert := v2alpha1.Certificate{
			ObjectMeta: v1.ObjectMeta{
				Name:   name,
				Labels: labels,
			},
			Spec: v2alpha1.CertificateSpec{
				Ca:      nextCAName,
				Client:  true,
				Subject: peer.Id + "-" + prefix,
			},
		}
		log.Printf("creating new client Certificate %s signed by %s", name, nextCAName)
		if _, err := skupperCli.Certificates(cli.Namespace).Create(ctx, &cert, v1.CreateOptions{}); err != nil {
			log.Fatal(err)
		}
	}
	for _, peer := range peerSites {
		certName := prefix + "-client-" + peer.Id
		fileName := path.Join(certdir, "client-site-secret-"+peer.Name+"-"+peer.Id+".yaml")
		linkName := certName
		for _, link := range peer.Links {
			if link.RemoteSiteId == siteID {
				linkName = link.Name
				break
			}
		}
		utils.RetryErrorWithContext(ctx, time.Millisecond*500, func() error {
			_, err := secrets.Get(ctx, certName, v1.GetOptions{})
			return err
		})
		clientSecret, err := secrets.Get(ctx, certName, v1.GetOptions{})
		if err != nil {
			log.Fatal(err)
		}
		clientSecret.ObjectMeta = v1.ObjectMeta{
			Name:      linkName,
			Namespace: peer.Namespace,
		}
		clientSecret.TypeMeta.APIVersion = "v1"
		clientSecret.Kind = "Secret"
		clientSecret.Data["ca.crt"] = trustBundle.Bytes()
		log.Printf("writing new client Secret for site %s to %s", peer.Name, fileName)
		file, err := os.Create(fileName)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()
		scheme := runtime.NewScheme()
		enc := json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{
			Yaml:   true,
			Pretty: true,
		})
		if err := enc.Encode(clientSecret, file); err != nil {
			log.Fatal(err)
		}
		file.Close()
	}
	fmt.Println("[manual step] Apply secrets to remote sites, then press enter to continue.")
	fmt.Scanln()

	log.Printf("replacing skupper-site-ca Certificate Secret with next CA %s", nextCAName)
	currCASecret, err = secrets.Get(ctx, ca.Name, v1.GetOptions{})
	if err != nil {
		log.Fatal(err)
	}
	currCASecret.Data = nextCASecret.Data
	if _, err := secrets.Update(ctx, currCASecret, v1.UpdateOptions{}); err != nil {
		log.Fatal(err)
	}
	log.Printf("replacing skupper-site-server Certificate Secret with next %s", nextServerName)
	nextServerCertSecret, err := secrets.Get(ctx, nextServerName, v1.GetOptions{})
	if err != nil {
		log.Fatal(err)
	}
	currServerCertSecret, err := secrets.Get(ctx, "skupper-site-server", v1.GetOptions{})
	if err != nil {
		log.Fatal(err)
	}
	currServerCertSecret.Data = nextServerCertSecret.Data
	if _, err := secrets.Update(ctx, currServerCertSecret, v1.UpdateOptions{}); err != nil {
		log.Fatal(err)
	}

	log.Printf("cleaning up rotation certificates for run: %s", prefix)
	err = skupperCli.Certificates(cli.Namespace).DeleteCollection(ctx, v1.DeleteOptions{}, v1.ListOptions{
		LabelSelector: "internal.skupper.io/hack-rotate-prefix=" + prefix,
	})
	if err != nil {
		log.Fatal(err)
	}
}

func getPrefix() string {
	var buf [2]byte
	io.ReadFull(rand.Reader, buf[:])
	return hex.EncodeToString(buf[:])
}

func GetActiveSite(ctx context.Context, namespace string, cli skupperv2alpha1.SkupperV2alpha1Interface) (*v2alpha1.Site, error) {
	sites, err := cli.Sites(namespace).List(ctx, v1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, site := range sites.Items {
		readyCond := meta.FindStatusCondition(site.Status.Conditions, v2alpha1.CONDITION_TYPE_READY)
		if readyCond == nil {
			continue
		}
		if readyCond.Status == v1.ConditionTrue {
			return site.DeepCopy(), nil
		}
	}
	return nil, fmt.Errorf("no active sites found out of %d sites", len(sites.Items))
}
