"""Opt-in live acceptance of the built gateway, using an empty isolated profile.

Runs only tiny public prompts; never reads the user's config, accounts or API key.
Usage: python tools/test_free_gateways.py --live
"""
import argparse
import http.client
import json
import secrets
import socket
import subprocess
import tempfile
import time
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--exe', type=Path, default=Path(__file__).resolve().parents[1] / 'wb2api.exe')
    parser.add_argument('--live', action='store_true', help='Contact the two public free providers.')
    parser.add_argument('--result', type=Path, help='Save successful acceptance evidence as JSON (no credentials).')
    args = parser.parse_args()
    if not args.live:
        parser.error('Pass --live to authorize the public network acceptance calls.')
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    key = secrets.token_urlsafe(24)

    def request(path, payload=None, authenticated=True):
        connection = http.client.HTTPConnection('127.0.0.1', port, timeout=75)
        headers = {'Content-Type': 'application/json'}
        if authenticated:
            headers['Authorization'] = f'Bearer {key}'
        connection.request('POST' if payload is not None else 'GET', path,
                           json.dumps(payload) if payload is not None else None, headers)
        response = connection.getresponse()
        status, content_type = response.status, response.getheader('Content-Type', '')
        body = response.read().decode('utf-8')
        connection.close()
        return status, content_type, body

    result = {}
    with tempfile.TemporaryDirectory(prefix='wb2api-free-smoke-') as folder:
        profile = Path(folder)
        (profile / 'auths').mkdir()
        config = {
            'listen': f'127.0.0.1:{port}', 'api_key': key,
            'auth_dir': './auths', 'state_file': './data/state.json',
            'global': {'enabled': False},
            'schedule': {f'{name}_enabled': False for name in
                         ('checkin', 'travel', 'activity', 'keepalive', 'school', 'cat')},
        }
        (profile / 'config.json').write_text(json.dumps(config), encoding='utf-8')
        with (profile / 'gateway.log').open('wb') as logs:
            process = subprocess.Popen([str(args.exe.resolve()), '-config', 'config.json'],
                                       cwd=profile, stdout=logs, stderr=logs,
                                       creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
            try:
                for attempt in range(40):
                    try:
                        result['health_status'] = request('/healthz')[0]
                        break
                    except OSError:
                        if process.poll() is not None:
                            raise AssertionError('Built gateway exited during startup')
                        time.sleep(.25)
                else:
                    raise AssertionError('Built gateway did not start')
                assert request('/v1/models', authenticated=False)[0] == 401
                result['local_auth'] = 'passed'
                status, _, body = request('/v1/models')
                assert status == 200
                models = json.loads(body)['data']
                result['models'] = {provider: sum(m['id'].startswith(provider + ':') for m in models)
                                    for provider in ('opencode', 'kilo')}
                for provider, count in result['models'].items():
                    if count == 0:
                        _, _, snapshot = request('/status')
                        state = next((item for item in json.loads(snapshot).get('free_gateways', [])
                                      if item['id'] == provider), {})
                        raise AssertionError(f'{provider}: live model catalog unavailable: {state.get("last_error", "unknown error")}')
                for provider in ('opencode', 'kilo'):
                    invalid = {'model': provider + ':this-model-is-not-free',
                               'messages': [{'role': 'user', 'content': 'Reply OK'}]}
                    assert request('/v1/chat/completions', invalid)[0] == 404
                result['unlisted_models_rejected'] = True
                kilo_ids = {m['id'] for m in models if m['id'].startswith('kilo:')}
                candidate = next((mid for mid in (
                    'kilo:cohere/north-mini-code:free', 'kilo:liquid/lfm-2.5-1.2b-thinking:free',
                ) if mid in kilo_ids), None)
                assert candidate, 'Known small free test model no longer listed; choose one explicitly'
                prompt = {'messages': [{'role': 'user', 'content': 'Reply with exactly OK.'}],
                          'max_tokens': 32, 'reasoning': {'enabled': False}}
                for stream in (False, True):
                    status, content_type, body = request('/v1/chat/completions',
                                                        dict(prompt, model=candidate, stream=stream))
                    assert status == 200, f'Kilo returned HTTP {status}'
                    if stream:
                        assert 'text/event-stream' in content_type
                        assert '[DONE]' in body and '"choices"' in body
                        result['kilo_stream'] = 'passed'
                    else:
                        document = json.loads(body)
                        assert document['choices'][0]['message'].get('content')
                        result['kilo_nonstream'] = 'passed'
                opencode_id = 'opencode:deepseek-v4.1-flash-free'
                assert opencode_id in {m['id'] for m in models}, 'Requested OpenCode model is no longer in the free catalog'
                opencode_prompt = {
                    'messages': [{'role': 'user', 'content': 'Reply with exactly OK. Do not use tools.'}],
                    'max_tokens': 256, 'reasoning_effort': 'low',
                }
                for stream in (False, True):
                    status, _, body = request('/v1/chat/completions',
                                              dict(opencode_prompt, model=opencode_id, stream=stream))
                    assert status == 200, f'OpenCode inference failed: HTTP {status}: {body[:500]}'
                    if stream:
                        frames = [json.loads(line[5:].strip()) for line in body.splitlines()
                                  if line.startswith('data:') and line[5:].strip() != '[DONE]']
                        assert not any(frame.get('error') for frame in frames), 'OpenCode stream contains an error'
                        content = ''.join(choice.get('delta', {}).get('content') or ''
                                          for frame in frames for choice in frame.get('choices', []))
                        assert content.strip() and '[DONE]' in body, 'OpenCode stream never completed a visible response'
                        result['opencode_stream'] = 'passed'
                    else:
                        document = json.loads(body)
                        assert document.get('choices') and document['choices'][0]['message'].get('content'), 'OpenCode returned an empty completion'
                        result['opencode_nonstream'] = 'passed'

                # Deliberately collide with the upstream read slot. The caller must
                # still receive its own tool name, schema and original call ID.
                tools = [{'type': 'function', 'function': {
                    'name': 'read', 'description': 'Return the supplied integer value.',
                    'parameters': {'type': 'object', 'properties': {'value': {'type': 'integer'}},
                                   'required': ['value'], 'additionalProperties': False},
                }}]
                messages = [{'role': 'user', 'content': 'Call the provided integer tool with value 7. Use the tool whose description is Return the supplied integer value.'}]
                status, _, body = request('/v1/chat/completions', {
                    'model': opencode_id, 'stream': False, 'max_tokens': 512,
                    'reasoning_effort': 'low', 'messages': messages, 'tools': tools,
                })
                assert status == 200, f'OpenCode caller tool failed: HTTP {status}: {body[:500]}'
                message = json.loads(body)['choices'][0]['message']
                calls = message.get('tool_calls') or []
                assert calls and all(call['function']['name'] == 'read' for call in calls), 'Caller tool was lost or leaked a compatibility tool'
                assert json.loads(calls[0]['function']['arguments'])['value'] == 7
                assert 'read_client' not in json.dumps(calls), 'Internal alias leaked in a callable tool name'
                result['opencode_tool_name_collision'] = 'passed'
                messages += [message]
                messages += [{'role': 'tool', 'tool_call_id': call['id'], 'content': '7'} for call in calls]
                messages += [{'role': 'user', 'content': 'Now reply with exactly OK. Do not call any tool.'}]
                status, _, body = request('/v1/chat/completions', {
                    'model': opencode_id, 'stream': False, 'max_tokens': 256,
                    'reasoning_effort': 'low', 'messages': messages, 'tools': tools,
                    'tool_choice': 'none',
                })
                assert status == 200, f'OpenCode tool-result history failed: HTTP {status}: {body[:500]}'
                message = json.loads(body)['choices'][0]['message']
                assert message.get('content') and not message.get('tool_calls'), 'Tool-result round trip failed'
                result['opencode_tool_roundtrip'] = 'passed'
                status, _, body = request('/status')
                assert status == 200
                snapshot = json.loads(body)
                assert snapshot['total'] == 0
                assert all(provider['inference_verified'] and provider['available']
                           for provider in snapshot['free_gateways']), 'A listed source was never verified by successful inference'
                result['free_gateways'] = snapshot['free_gateways']
                status, _, body = request('/v1/usage/live')
                live = json.loads(body)
                assert status == 200 and live['active_requests'] == 0
                result['idle_usage'] = 'passed'
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
    evidence = json.dumps(result, ensure_ascii=False, indent=2)
    if args.result:
        args.result.parent.mkdir(parents=True, exist_ok=True)
        args.result.write_text(evidence + '\n', encoding='utf-8')
    print(evidence)


if __name__ == '__main__':
    main()
