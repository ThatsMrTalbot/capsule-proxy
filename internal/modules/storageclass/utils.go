package storageclass

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"

	capsulev1alpha1 "github.com/clastix/capsule/api/v1alpha1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	utils "github.com/clastix/capsule-proxy/internal/modules/utils"
)

const (
	storageClassListingAnnotation  = "capsule.clastix.io/enable-storageclass-listing"
	storageClassUpdateAnnotation   = "capsule.clastix.io/enable-storageclass-update"
	storageClassDeletionAnnotation = "capsule.clastix.io/enable-storageclass-deletion"
)

func getStorageClasses(req *http.Request, tenants *capsulev1alpha1.TenantList) (exact []string, regex []*regexp.Regexp) {
	for _, tenant := range tenants.Items {
		var ok bool

		switch req.Method {
		case http.MethodGet:
			ok = utils.IsAnnotationTrue(tenant, storageClassListingAnnotation)
		case http.MethodPut, http.MethodPatch:
			ok = utils.IsAnnotationTrue(tenant, storageClassListingAnnotation)
			ok = ok && utils.IsAnnotationTrue(tenant, storageClassUpdateAnnotation)
		case http.MethodDelete:
			ok = utils.IsAnnotationTrue(tenant, storageClassListingAnnotation)
			ok = ok && utils.IsAnnotationTrue(tenant, storageClassDeletionAnnotation)
		default:
			break
		}

		if ok {
			sc := tenant.Spec.StorageClasses
			if sc == nil {
				continue
			}

			if len(sc.Exact) > 0 {
				exact = append(exact, sc.Exact...)
			}

			if r := sc.Regex; len(r) > 0 {
				regex = append(regex, regexp.MustCompile(r))
			}
		}
	}

	sort.SliceStable(exact, func(i, j int) bool {
		return exact[i] < exact[0]
	})

	return exact, regex
}

func getStorageClassSelector(classes *storagev1.StorageClassList, exact []string, regex []*regexp.Regexp) (*labels.Requirement, error) {
	isStorageClassRegexed := func(name string, regex []*regexp.Regexp) bool {
		for _, r := range regex {
			if r.MatchString(name) {
				return true
			}
		}

		return false
	}

	var names []string

	for _, s := range classes.Items {
		if isStorageClassRegexed(s.GetName(), regex) {
			names = append(names, s.GetName())

			continue
		}

		if f := sort.SearchStrings(exact, s.GetName()); f < len(exact) && exact[f] == s.GetName() {
			names = append(names, s.GetName())
		}
	}

	if len(names) > 0 {
		return labels.NewRequirement("name", selection.In, names)
	}

	return nil, fmt.Errorf("cannot create LabelSelector for the requested StorageClass requirement")
}
