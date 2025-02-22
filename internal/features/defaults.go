package features

func Default() UserFeatures {
	return UserFeatures{
		// NOTE: ensure all nested objects are fully populated
		ResourceGroup: ResourceGroupFeatures{
			PreventDeletionIfContainsResources: false,
		},
	}
}
