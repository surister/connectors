from logging import Logger
from datetime import datetime, UTC
from typing import (
    TYPE_CHECKING,
    Annotated,
    Any,
    AsyncGenerator,
    Callable,
    Generic,
    Literal,
    TypeVar,
)
import json

from estuary_cdk.flow import (
    AccessToken,
    AuthorizationCodeFlowOAuth2Credentials,
    OAuth2Spec,
)

from estuary_cdk.capture.common import (
    BaseDocument,
    LogCursor,
    PageCursor,
    ResourceState,
)
from estuary_cdk.capture.common import (
    ConnectorState as GenericConnectorState,
)
from estuary_cdk.http import HTTPSession
from pydantic import AwareDatetime, BaseModel, Field, field_validator

OAUTH2_SPEC = OAuth2Spec(
    provider="monday",
    authUrlTemplate=(
        "https://auth.monday.com/oauth2/authorize?"
        r"client_id={{#urlencode}}{{{ client_id }}}{{/urlencode}}"
        r"&state={{#urlencode}}{{{ state }}}{{/urlencode}}"
    ),
    accessTokenUrlTemplate="https://auth.monday.com/oauth2/token",
    accessTokenHeaders={"content-type": "application/x-www-form-urlencoded"},
    accessTokenBody=(
        r"client_id={{#urlencode}}{{{ client_id }}}{{/urlencode}}"
        r"&client_secret={{#urlencode}}{{{ client_secret }}}{{/urlencode}}"
        r"&code={{#urlencode}}{{{ code }}}{{/urlencode}}"
    ),
    accessTokenResponseMap={
        "access_token": "/access_token",
    },
)


if TYPE_CHECKING:
    OAuth2Credentials = AuthorizationCodeFlowOAuth2Credentials
else:
    OAuth2Credentials = AuthorizationCodeFlowOAuth2Credentials.for_provider(
        OAUTH2_SPEC.provider
    )


# TODO(justin): Remove AccessToken and add OAuth2Credentials to EndpointConfig
# when the OAuth app is created and approved by Monday.com for public use.
class EndpointConfig(BaseModel):
    credentials: AccessToken = Field(
        title="Authentication",
        discriminator="credentials_title",
    )

    class Advanced(BaseModel, extra="forbid"):
        limit: Annotated[
            int,
            Field(
                description="Limit used in queries for incremental streams. This should be left as the default value unless connector errors indicate a smaller limit is required.",
                title="Limit",
                default=5,
                gt=0,
            ),
        ]

    advanced: Advanced = Field(
        default_factory=Advanced,  # type: ignore
        title="Advanced Config",
        description="Advanced settings for the connector.",
        json_schema_extra={"advanced": True},
    )


ConnectorState = GenericConnectorState[ResourceState]
ResponseObject = TypeVar("ResponseObject", bound=BaseModel)


class FullRefreshResource(BaseDocument, extra="allow"):
    pass


class IncrementalResource(BaseDocument, extra="allow"):
    id: str
    updated_at: AwareDatetime


class GraphQLErrorLocation(BaseModel, extra="forbid"):
    line: int
    column: int


class GraphQLError(BaseModel, extra="forbid"):
    message: str
    locations: list[GraphQLErrorLocation] | None = None
    path: list[Any] | None = None
    extensions: dict[str, Any] | None = None


class GraphQLResponse(BaseModel, Generic[ResponseObject], extra="forbid"):
    data: ResponseObject | None = None
    errors: list[GraphQLError] | None = None


class ActivityLog(BaseModel, extra="allow"):
    data: dict[str, Any] = Field(default_factory=dict)

    @field_validator("data", mode="before")
    @classmethod
    def parse_json_data(cls, v: Any) -> dict[str, Any]:
        if isinstance(v, str):
            return json.loads(v)
        return v


class BoardActivityLogs(BaseModel, extra="allow"):
    activity_logs: list[ActivityLog]


class ActivityLogsResponse(BaseModel, extra="forbid"):
    boards: list[BoardActivityLogs]


class Tag(BaseDocument, extra="allow"):
    pass


class Board(BaseDocument, extra="allow"):
    updated_at: AwareDatetime


class BoardsResponse(BaseModel, extra="forbid"):
    boards: list[Board]


class ParentItemRef(BaseModel, extra="forbid"):
    id: str


class Item(BaseDocument, extra="allow"):
    id: str
    parent_item: ParentItemRef | None = None
    updated_at: AwareDatetime


class ItemsPage(BaseModel, extra="forbid"):
    cursor: str | None = None
    items: list[Item]


class BoardItems(BaseModel, extra="forbid"):
    id: str
    items_page: ItemsPage


class ItemsByBoardResponse(BaseModel, extra="forbid"):
    boards: list[BoardItems] | None = Field(default_factory=list)


class ItemsByBoardPageResponse(BaseModel, extra="forbid"):
    next_items_page: ItemsPage


class ItemsByIdResponse(BaseModel, extra="forbid"):
    items: list[Item] | None = Field(default_factory=list)


class Team(BaseDocument, extra="allow"):
    pass


class TeamsResponse(BaseModel, extra="forbid"):
    teams: list[Team]


class User(BaseDocument, extra="allow"):
    id: str
    pass


class UsersResponse(BaseModel, extra="forbid"):
    users: list[User]


class TagsResponse(BaseModel, extra="forbid"):
    tags: list[Tag]


FullRefreshResourceFetchFn = Callable[
    [HTTPSession, int, Logger], AsyncGenerator[BaseDocument, None]
]

IncrementalResourceFetchChangesFn = Callable[
    [HTTPSession, int, Logger, LogCursor],
    AsyncGenerator[BaseDocument | LogCursor, None],
]

IncrementalResourceFetchPageFn = Callable[
    [HTTPSession, int, Logger, PageCursor, LogCursor],
    AsyncGenerator[BaseDocument | PageCursor, None],
]
