package v1alpha1

import "k8s.io/apimachinery/pkg/runtime"

func (in *TunnelRoute) DeepCopyObject() runtime.Object {
	out := new(TunnelRoute)
	in.DeepCopyInto(out)
	return out
}

func (in *TunnelRoute) DeepCopyInto(out *TunnelRoute) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

func (in *TunnelRouteList) DeepCopyObject() runtime.Object {
	out := new(TunnelRouteList)
	in.DeepCopyInto(out)
	return out
}

func (in *TunnelRouteList) DeepCopyInto(out *TunnelRouteList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]TunnelRoute, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *TunnelRouteSpec) DeepCopyInto(out *TunnelRouteSpec) {
	*out = *in
	if in.Rules != nil {
		out.Rules = make([]TunnelRouteRule, len(in.Rules))
		for i := range in.Rules {
			in.Rules[i].DeepCopyInto(&out.Rules[i])
		}
	}
}

func (in *TunnelRouteRule) DeepCopyInto(out *TunnelRouteRule) {
	*out = *in
	if in.OriginRequest != nil {
		out.OriginRequest = new(TunnelRouteOriginRequest)
		*out.OriginRequest = *in.OriginRequest
	}
}
