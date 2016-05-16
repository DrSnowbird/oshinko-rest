package clusters

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the swagger generate command

import (
	"net/http"

	"github.com/go-openapi/errors"
	"github.com/go-openapi/runtime/middleware"

	strfmt "github.com/go-openapi/strfmt"
)

// NewFindSingleClusterParams creates a new FindSingleClusterParams object
// with the default values initialized.
func NewFindSingleClusterParams() FindSingleClusterParams {
	var ()
	return FindSingleClusterParams{}
}

// FindSingleClusterParams contains all the bound params for the find single cluster operation
// typically these are obtained from a http.Request
//
// swagger:parameters findSingleCluster
type FindSingleClusterParams struct {

	// HTTP Request Object
	HTTPRequest *http.Request

	/*Name of the cluster
	  Required: true
	  In: path
	*/
	Name string
}

// BindRequest both binds and validates a request, it assumes that complex things implement a Validatable(strfmt.Registry) error interface
// for simple values it will use straight method calls
func (o *FindSingleClusterParams) BindRequest(r *http.Request, route *middleware.MatchedRoute) error {
	var res []error
	o.HTTPRequest = r

	rName, rhkName, _ := route.Params.GetOK("name")
	if err := o.bindName(rName, rhkName, route.Formats); err != nil {
		res = append(res, err)
	}

	if len(res) > 0 {
		return errors.CompositeValidationError(res...)
	}
	return nil
}

func (o *FindSingleClusterParams) bindName(rawData []string, hasKey bool, formats strfmt.Registry) error {
	var raw string
	if len(rawData) > 0 {
		raw = rawData[len(rawData)-1]
	}

	o.Name = raw

	return nil
}