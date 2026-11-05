// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Security groups (ADR 0012): `feos.network.v1.SecurityGroupService`, the
//! groups kept on disk, and the compiler that turns them and the node's
//! pods into sets of addresses and rules for nftables. Name tables (ADR
//! 0010, ADR 0018): `feos.network.v1.NameService`, the tables kept on disk,
//! from which the resolver answers the pods that name them.

mod group;
mod kept;
mod name_service;
mod name_store;
mod names;
mod policy;
mod prefix;
mod ranges;
mod registry;
mod service;
mod store;

pub use group::{check_id, Protocol};
pub use name_service::NameTables;
pub use names::check_table_id;
pub use policy::{
    compile, delta, AddressRange, AddressSet, Allow, Delta, Pod, PodPolicy, Policy, SetChange,
    DENY4, DENY6, INTERNET4, INTERNET6,
};
pub use prefix::{Family, Prefix};
pub use registry::always_denied;
pub use service::SecurityGroups;
pub use store::Store;

use feos_proto::network_service::name_service_server::NameService;
use feos_proto::network_service::security_group_service_server::SecurityGroupService;
use feos_proto::network_service::{
    DeleteNameTableRequest, DeleteNameTableResponse, DeleteSecurityGroupRequest,
    DeleteSecurityGroupResponse, GetNameTableRequest, GetSecurityGroupRequest,
    ListNameTablesRequest, ListNameTablesResponse, ListSecurityGroupsRequest,
    ListSecurityGroupsResponse, NameTable, PutNameTableRequest, PutNameTableResponse,
    PutSecurityGroupRequest, PutSecurityGroupResponse, SecurityGroup, UpdateNameTableRequest,
    UpdateNameTableResponse, UpdateSecurityGroupRequest, UpdateSecurityGroupResponse,
};
use tonic::{Request, Response, Status};

/// Where the security groups are kept: a file each.
pub const SECURITY_GROUP_DIR: &str = "/var/lib/feos/security-groups";
/// Where the name tables are kept: a file each.
pub const NAME_TABLE_DIR: &str = "/var/lib/feos/name-tables";

#[tonic::async_trait]
impl SecurityGroupService for SecurityGroups {
    async fn put_security_group(
        &self,
        request: Request<PutSecurityGroupRequest>,
    ) -> Result<Response<PutSecurityGroupResponse>, Status> {
        let group = request
            .into_inner()
            .group
            .ok_or_else(|| Status::invalid_argument("no security group"))?;
        self.put(group).await?;
        Ok(Response::new(PutSecurityGroupResponse {}))
    }

    async fn update_security_group(
        &self,
        request: Request<UpdateSecurityGroupRequest>,
    ) -> Result<Response<UpdateSecurityGroupResponse>, Status> {
        self.update(request.into_inner()).await?;
        Ok(Response::new(UpdateSecurityGroupResponse {}))
    }

    async fn delete_security_group(
        &self,
        request: Request<DeleteSecurityGroupRequest>,
    ) -> Result<Response<DeleteSecurityGroupResponse>, Status> {
        self.delete(&request.into_inner().id).await?;
        Ok(Response::new(DeleteSecurityGroupResponse {}))
    }

    async fn get_security_group(
        &self,
        request: Request<GetSecurityGroupRequest>,
    ) -> Result<Response<SecurityGroup>, Status> {
        Ok(Response::new(self.get(&request.into_inner().id)?))
    }

    async fn list_security_groups(
        &self,
        _request: Request<ListSecurityGroupsRequest>,
    ) -> Result<Response<ListSecurityGroupsResponse>, Status> {
        Ok(Response::new(ListSecurityGroupsResponse {
            groups: self.list(),
        }))
    }
}

#[tonic::async_trait]
impl NameService for NameTables {
    async fn put_name_table(
        &self,
        request: Request<PutNameTableRequest>,
    ) -> Result<Response<PutNameTableResponse>, Status> {
        let table = request
            .into_inner()
            .table
            .ok_or_else(|| Status::invalid_argument("no name table"))?;
        self.put(table).await?;
        Ok(Response::new(PutNameTableResponse {}))
    }

    async fn update_name_table(
        &self,
        request: Request<UpdateNameTableRequest>,
    ) -> Result<Response<UpdateNameTableResponse>, Status> {
        self.update(request.into_inner()).await?;
        Ok(Response::new(UpdateNameTableResponse {}))
    }

    async fn delete_name_table(
        &self,
        request: Request<DeleteNameTableRequest>,
    ) -> Result<Response<DeleteNameTableResponse>, Status> {
        self.delete(&request.into_inner().id).await?;
        Ok(Response::new(DeleteNameTableResponse {}))
    }

    async fn get_name_table(
        &self,
        request: Request<GetNameTableRequest>,
    ) -> Result<Response<NameTable>, Status> {
        Ok(Response::new(self.get(&request.into_inner().id)?))
    }

    async fn list_name_tables(
        &self,
        _request: Request<ListNameTablesRequest>,
    ) -> Result<Response<ListNameTablesResponse>, Status> {
        Ok(Response::new(ListNameTablesResponse {
            tables: self.list(),
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tonic::Code;

    #[tokio::test]
    async fn the_service_serves_the_groups() {
        let dir = tempfile::tempdir().unwrap();
        let groups = SecurityGroups::open(dir.path()).unwrap();
        let group = SecurityGroup {
            id: "web".to_string(),
            version: 1,
            rules: vec![],
            members: vec!["fd00::1".to_string()],
        };
        let put = PutSecurityGroupRequest { group: None };
        let error = groups
            .put_security_group(Request::new(put))
            .await
            .unwrap_err();
        assert_eq!(error.code(), Code::InvalidArgument);
        let put = PutSecurityGroupRequest {
            group: Some(group.clone()),
        };
        groups.put_security_group(Request::new(put)).await.unwrap();

        let update = UpdateSecurityGroupRequest {
            id: "web".to_string(),
            from_version: 1,
            version: 2,
            remove_members: vec!["fd00::1".to_string()],
            ..Default::default()
        };
        groups
            .update_security_group(Request::new(update))
            .await
            .unwrap();
        let get = GetSecurityGroupRequest {
            id: "web".to_string(),
        };
        let got = groups.get_security_group(Request::new(get)).await.unwrap();
        assert_eq!((got.get_ref().version, got.get_ref().members.len()), (2, 0));

        let listed = groups
            .list_security_groups(Request::new(ListSecurityGroupsRequest {}))
            .await
            .unwrap();
        assert_eq!(listed.get_ref().groups.len(), 1);
        groups.set_in_use(|id| id == "web");
        let delete = || {
            Request::new(DeleteSecurityGroupRequest {
                id: "web".to_string(),
            })
        };
        let error = groups.delete_security_group(delete()).await.unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition);
        groups.set_in_use(|_| false);
        groups.delete_security_group(delete()).await.unwrap();
        let error = groups.delete_security_group(delete()).await.unwrap_err();
        assert_eq!(error.code(), Code::NotFound);
    }

    #[tokio::test]
    async fn the_service_serves_the_name_tables() {
        use feos_proto::network_service::NameEntry;
        let dir = tempfile::tempdir().unwrap();
        let tables = NameTables::open(dir.path()).unwrap();
        let put = PutNameTableRequest { table: None };
        let error = tables.put_name_table(Request::new(put)).await.unwrap_err();
        assert_eq!(error.code(), Code::InvalidArgument);
        let table = NameTable {
            id: "env-1".to_string(),
            version: 1,
            entries: vec![NameEntry {
                name: "web".to_string(),
                addresses: vec!["fd00::1".to_string()],
            }],
            includes: vec![],
        };
        let put = PutNameTableRequest { table: Some(table) };
        tables.put_name_table(Request::new(put)).await.unwrap();

        let update = UpdateNameTableRequest {
            id: "env-1".to_string(),
            from_version: 1,
            version: 2,
            remove_names: vec!["web".to_string()],
            ..Default::default()
        };
        tables
            .update_name_table(Request::new(update))
            .await
            .unwrap();
        let get = GetNameTableRequest {
            id: "env-1".to_string(),
        };
        let got = tables.get_name_table(Request::new(get)).await.unwrap();
        assert_eq!((got.get_ref().version, got.get_ref().entries.len()), (2, 0));
        let listed = tables
            .list_name_tables(Request::new(ListNameTablesRequest {}))
            .await
            .unwrap();
        assert_eq!(listed.get_ref().tables.len(), 1);
        tables.set_in_use(|id| id == "env-1");
        let delete = || {
            Request::new(DeleteNameTableRequest {
                id: "env-1".to_string(),
            })
        };
        let error = tables.delete_name_table(delete()).await.unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition);
        tables.set_in_use(|_| false);
        tables.delete_name_table(delete()).await.unwrap();
        let error = tables.delete_name_table(delete()).await.unwrap_err();
        assert_eq!(error.code(), Code::NotFound);
    }
}
