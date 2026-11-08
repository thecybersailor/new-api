package authz

var (
	CatalogRead      = Permission{Resource: "catalog", Action: "read"}
	CatalogEdit      = Permission{Resource: "catalog", Action: "edit"}
	CatalogPublish   = Permission{Resource: "catalog", Action: "publish"}
	CatalogAuthorize = Permission{Resource: "catalog", Action: "authorize"}
)

func init() {
	RegisterResource(ResourceDefinition{Resource: "catalog", LabelKey: "Integration catalog", Actions: []ActionDefinition{
		{Action: "read", LabelKey: "View integration catalog", DescriptionKey: "View catalog configuration and audit history.", DefaultRoles: []string{BuiltInRoleAdmin}},
		{Action: "edit", LabelKey: "Edit catalog drafts", DescriptionKey: "Edit non-executable catalog drafts."},
		{Action: "publish", LabelKey: "Publish catalog operations", DescriptionKey: "Publish or retire supplier operations."},
		{Action: "authorize", LabelKey: "Manage catalog authorization", DescriptionKey: "Manage integration bindings and explicit authorization."},
	}})
}
