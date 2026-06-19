from typing import Literal, Optional, Union

import pydantic
from base import Base


class HostManagementSsh(Base):
    kind: Literal["hostManagement"]
    hostManagementType: Literal["ssh"]
    hostId: str
    ipAddress: pydantic.IPvAnyAddress
    port: int = 22
    username: str = "root"


class HostManagementSshPassword(HostManagementSsh):
    password: str


class HostManagementSshKey(HostManagementSsh):
    privateKey: str
    passphrase: Optional[str] = None


HostManagement = Union[HostManagementSshPassword, HostManagementSshKey]
