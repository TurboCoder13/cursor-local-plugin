package provider

import "strings"

type accountRecord struct {
	File       managedFile
	Credential credentials
	Error      error
}
type accountGroup struct {
	Records    []accountRecord
	Identities map[string]bool
}

// Merge connected identity groups, including bridges between an ID and email.
func groupAccounts(records []accountRecord) []accountGroup {
	var groups []accountGroup
	for _, record := range records {
		identities := make(map[string]bool)
		if record.Error == nil {
			identities["id:"+strings.ToLower(record.Credential.AccountID)] = true
			if record.Credential.Email != "" {
				identities["email:"+strings.ToLower(record.Credential.Email)] = true
			}
		}
		combined := accountGroup{Records: []accountRecord{record}, Identities: identities}
		var retained []accountGroup
		for _, group := range groups {
			overlap := false
			for key := range identities {
				overlap = overlap || group.Identities[key]
			}
			if !overlap {
				retained = append(retained, group)
				continue
			}
			combined.Records = append(group.Records, combined.Records...)
			for key := range group.Identities {
				combined.Identities[key] = true
			}
		}
		groups = append(retained, combined)
	}
	return groups
}
