/*
Copyright 2020 The Kubernetes Authors.

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

package webhook

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"

	popclientset "github.com/kubernetes-csi/external-provisioner/client/clientset/versioned"
	popinformers "github.com/kubernetes-csi/external-provisioner/client/informers/externalversions"
	ctrl "github.com/kubernetes-csi/external-provisioner/pkg/controller"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/admission/v1"
	"k8s.io/api/admission/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

var (
	certFile   string
	keyFile    string
	port       int
	master     string
	kubeconfig string
)

// CmdWebhook is used by Cobra.
var CmdWebhook = &cobra.Command{
	Use:   "validation-webhook",
	Short: "Starts a HTTPS server, uses ValidatingAdmissionWebhook to perform validation on PVC data sources",
	Long: `Starts a HTTPS server, uses ValidatingAdmissionWebhook to perform validation on PVC data sources.
After deploying it to Kubernetes cluster, the Administrator needs to create a ValidatingWebhookConfiguration
in the Kubernetes cluster to register remote webhook admission controllers. Phase one of https://github.com/kubernetes/enhancements/blob/master/keps/sig-storage/177-volume-snapshot/tighten-validation-webhook-crd.md`,
	Args: cobra.MaximumNArgs(0),
	Run:  main,
}

func init() {
	CmdWebhook.Flags().StringVar(&certFile, "tls-cert-file", "",
		"File containing the x509 Certificate for HTTPS. (CA cert, if any, concatenated after server cert). Required.")
	CmdWebhook.Flags().StringVar(&keyFile, "tls-private-key-file", "",
		"File containing the x509 private key matching --tls-cert-file. Required.")
	CmdWebhook.Flags().IntVar(&port, "port", 443,
		"Secure port that the webhook listens on")
	CmdWebhook.Flags().StringVar(&master, "master", "",
		"Master URL to build a client config from. Either this or kubeconfig needs to be set if the provisioner is being run out of cluster.")
	CmdWebhook.Flags().StringVar(&kubeconfig, "kubeconfig", "",
		"Absolute path to the kubeconfig file. Either this or master needs to be set if the provisioner is being run out of cluster.")
	CmdWebhook.MarkFlagRequired("tls-cert-file")
	CmdWebhook.MarkFlagRequired("tls-private-key-file")
}

// admitv1beta1Func handles a v1beta1 admission
type admitv1beta1Func func(v1beta1.AdmissionReview) *v1beta1.AdmissionResponse

// admitv1beta1Func handles a v1 admission
type admitv1Func func(v1.AdmissionReview) *v1.AdmissionResponse

// admitHandler is a handler, for both validators and mutators, that supports multiple admission review versions
type admitHandler struct {
	mu         sync.Mutex
	populators []metav1.GroupKind
	v1beta1    admitv1beta1Func
	v1         admitv1Func
}

func newDelegateToV1AdmitHandler(f admitv1Func) admitHandler {
	return admitHandler{
		v1beta1: delegateV1beta1AdmitToV1(f),
		v1:      f,
	}
}

func delegateV1beta1AdmitToV1(f admitv1Func) admitv1beta1Func {
	return func(review v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
		in := v1.AdmissionReview{Request: convertAdmissionRequestToV1(review.Request)}
		out := f(in)
		return convertAdmissionResponseToV1beta1(out)
	}
}

// serve handles the http portion of a request prior to handing to an admit
// function
func serve(w http.ResponseWriter, r *http.Request, admit admitHandler) {
	var body []byte
	if r.Body == nil {
		msg := "Expected request body to be non-empty"
		klog.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
	}

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		msg := fmt.Sprintf("Request could not be decoded: %v", err)
		klog.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
	}
	body = data

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		msg := fmt.Sprintf("contentType=%s, expect application/json", contentType)
		klog.Errorf(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	klog.V(2).Info(fmt.Sprintf("handling request: %s", body))

	deserializer := codecs.UniversalDeserializer()
	obj, gvk, err := deserializer.Decode(body, nil, nil)
	if err != nil {
		msg := fmt.Sprintf("Request could not be decoded: %v", err)
		klog.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	var responseObj runtime.Object
	switch *gvk {
	case v1beta1.SchemeGroupVersion.WithKind("AdmissionReview"):
		requestedAdmissionReview, ok := obj.(*v1beta1.AdmissionReview)
		if !ok {
			msg := fmt.Sprintf("Expected v1beta1.AdmissionReview but got: %T", obj)
			klog.Errorf(msg)
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		responseAdmissionReview := &v1beta1.AdmissionReview{}
		responseAdmissionReview.SetGroupVersionKind(*gvk)
		responseAdmissionReview.Response = admit.v1beta1(*requestedAdmissionReview)
		responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
		responseObj = responseAdmissionReview
	case v1.SchemeGroupVersion.WithKind("AdmissionReview"):
		requestedAdmissionReview, ok := obj.(*v1.AdmissionReview)
		if !ok {
			msg := fmt.Sprintf("Expected v1.AdmissionReview but got: %T", obj)
			klog.Errorf(msg)
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		responseAdmissionReview := &v1.AdmissionReview{}
		responseAdmissionReview.SetGroupVersionKind(*gvk)
		responseAdmissionReview.Response = admit.v1(*requestedAdmissionReview)
		responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
		responseObj = responseAdmissionReview
	default:
		msg := fmt.Sprintf("Unsupported group version kind: %v", gvk)
		klog.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	klog.V(2).Info(fmt.Sprintf("sending response: %v", responseObj))
	respBytes, err := json.Marshal(responseObj)
	if err != nil {
		klog.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		klog.Error(err)
	}
}

func servePVCRequest(w http.ResponseWriter, r *http.Request) {
	serve(w, r, newDelegateToV1AdmitHandler(admitPVC))
}

func main(cmd *cobra.Command, args []string) {
	fmt.Println("Starting webhook server")
	config := Config{
		CertFile: certFile,
		KeyFile:  keyFile,
	}

	cfg, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if nil != err {
		klog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	// snapclientset.NewForConfig creates a new Clientset for VolumesnapshotV1beta1Client
	popClient, err := popclientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Failed to create snapshot client: %v", err)
	}

	factory := popinformers.NewSharedInformerFactory(popClient, ctrl.ResyncPeriodOfCsiNodeInformer)
	popInformer := factory.Populator().V1alpha1().VolumePopulators()
	popLister = popInformer.Lister()
	stopCh := make(chan struct{})
	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)

	http.HandleFunc("/pvc", servePVCRequest)
	http.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) { w.Write([]byte("ok")) })
	server := &http.Server{
		Addr:      fmt.Sprintf(":%d", port),
		TLSConfig: configTLS(config),
	}
	err = server.ListenAndServeTLS("", "")
	if err != nil {
		panic(err)
	}
}
