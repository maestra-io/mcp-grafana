import pytest
from mcp import ClientSession

from conftest import models
from utils import assert_mcp_eval, run_llm_tool_loop


pytestmark = pytest.mark.anyio


@pytest.mark.parametrize("model", models)
@pytest.mark.flaky(reruns=2)
async def test_elasticsearch_query_logs(
    model: str,
    mcp_client: ClientSession,
    mcp_transport: str,
):
    prompt = (
        "Can you query the Elasticsearch datasource for the last 10 log entries "
        "from the 'test-logs-2024' index? Show me the log messages and their severity levels."
    )
    final_content, tools_called, mcp_server = await run_llm_tool_loop(
        model, mcp_client, mcp_transport, prompt
    )

    assert_mcp_eval(
        prompt,
        final_content,
        tools_called,
        mcp_server,
        "Does the response contain specific log data that could only come from an Elasticsearch datasource? "
        "This could include log messages with levels like 'info', 'error', 'warn', or 'debug', "
        "service names like 'api-gateway' or 'auth-service', or HTTP status codes. "
        "The response should show evidence of real data rather than generic statements.",
        expected_tools="query_elasticsearch",
    )


@pytest.mark.parametrize("model", models)
@pytest.mark.flaky(reruns=2)
async def test_elasticsearch_query_errors(
    model: str,
    mcp_client: ClientSession,
    mcp_transport: str,
):
    prompt = (
        "Search for error-level logs in the Elasticsearch datasource using the 'test-logs-2024' index. "
        "Use the query 'level:error' to find them. What errors occurred?"
    )
    final_content, tools_called, mcp_server = await run_llm_tool_loop(
        model, mcp_client, mcp_transport, prompt
    )

    assert_mcp_eval(
        prompt,
        final_content,
        tools_called,
        mcp_server,
        "Does the response contain information about error-level log entries from Elasticsearch? "
        "It should reference specific error messages such as database timeouts or JSON parsing failures. "
        "The response should show evidence of real error data rather than generic statements.",
        expected_tools="query_elasticsearch",
    )


def _assert_opensearch_datasource_used(tools_called) -> None:
    """Assert query_elasticsearch was actually routed to the OpenSearch datasource.

    Without this check the OpenSearch tests would pass even if the LLM picked
    the Elasticsearch datasource, since both datasources contain the same
    seeded log data.
    """
    used_uids = [
        tc.args.get("datasourceUid")
        for tc in tools_called
        if tc.name == "query_elasticsearch"
    ]
    assert "opensearch" in used_uids, (
        f"Expected query_elasticsearch with datasourceUid='opensearch', "
        f"got datasourceUids={used_uids}"
    )


@pytest.mark.parametrize("model", models)
@pytest.mark.flaky(reruns=2)
async def test_opensearch_query_logs(
    model: str,
    mcp_client: ClientSession,
    mcp_transport: str,
):
    prompt = (
        "Can you query the OpenSearch datasource for the last 10 log entries "
        "from the 'test-logs-2024' index? Show me the log messages and their severity levels."
    )
    final_content, tools_called, mcp_server = await run_llm_tool_loop(
        model, mcp_client, mcp_transport, prompt
    )

    _assert_opensearch_datasource_used(tools_called)
    assert_mcp_eval(
        prompt,
        final_content,
        tools_called,
        mcp_server,
        "Does the response contain specific log data that could only come from an OpenSearch datasource? "
        "This could include log messages with levels like 'info', 'error', 'warn', or 'debug', "
        "service names like 'api-gateway' or 'auth-service', or HTTP status codes. "
        "The response should show evidence of real data rather than generic statements.",
        expected_tools="query_elasticsearch",
    )


@pytest.mark.parametrize("model", models)
@pytest.mark.flaky(reruns=2)
async def test_opensearch_query_errors(
    model: str,
    mcp_client: ClientSession,
    mcp_transport: str,
):
    prompt = (
        "Search for error-level logs in the OpenSearch datasource using the 'test-logs-2024' index. "
        "Use the query 'level:error' to find them. What errors occurred?"
    )
    final_content, tools_called, mcp_server = await run_llm_tool_loop(
        model, mcp_client, mcp_transport, prompt
    )

    _assert_opensearch_datasource_used(tools_called)
    assert_mcp_eval(
        prompt,
        final_content,
        tools_called,
        mcp_server,
        "Does the response contain information about error-level log entries from OpenSearch? "
        "It should reference specific error messages such as database timeouts or JSON parsing failures. "
        "The response should show evidence of real error data rather than generic statements.",
        expected_tools="query_elasticsearch",
    )
