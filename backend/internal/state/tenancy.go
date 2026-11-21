package state

// The multi-tenant install record (tenancy design K1) in the root meta table. A single-tenant
// install never has it: no record means single.
const (
	MetaTenancyMode = "tenancy_mode"
	MetaStaffOrgID  = "staff_org_id"
)

// TenancyRecord returns the recorded mode and staff org, "" when absent.
func (s *Store) TenancyRecord() (mode, staffOrg string, err error) {
	if mode, err = metaString(s.db, MetaTenancyMode); err == nil {
		staffOrg, err = metaString(s.db, MetaStaffOrgID)
	}
	return mode, staffOrg, err
}

// RecordTenancy writes the record in one transaction, refusing to replace either key, so a
// record is only ever written whole and once.
func (s *Store) RecordTenancy(mode, staffOrg string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, kv := range [][2]string{{MetaTenancyMode, mode}, {MetaStaffOrgID, staffOrg}} {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}
