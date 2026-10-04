#!/usr/bin/env ruby
# Short live Cursor subscription acceptance; never print credentials/error bodies.
require 'json'
require 'net/http'

require_relative 'environment'
settings = CursorLocal::Environment.new.parse!(ARGV)
raise 'at most one model ID is accepted' if ARGV.length > 1
key = File.read(settings.client_key).strip
model = ARGV.first || 'cursor/gpt-5.4-mini-none'
completion = ->(token, body) do
  response = settings.request('/v1/chat/completions', key: token, method: :post, body: JSON.generate(body), timeout: 120)
  raise "Cursor acceptance HTTP #{response.code}" unless response.code == '200'
  response.body
end
base = {'model' => model, 'max_completion_tokens' => 1024}
text = JSON.parse(completion.call(key, base.merge('messages' => [{'role' => 'user', 'content' => 'Reply with exactly PROXY_OK.'}])))
raise 'text acceptance returned no expected content' unless text.dig('choices', 0, 'message', 'content').to_s.include?('PROXY_OK')
puts "PASS: live Cursor text completion via #{model}."
STDOUT.flush
stream = completion.call(key, base.merge('stream' => true, 'messages' => [{'role' => 'user', 'content' => 'Reply with exactly STREAM_OK.'}]))
chunks = stream.lines.map { |line| JSON.parse(line.delete_prefix('data: ').strip) if line.start_with?('data: ') && !line.include?('[DONE]') }.compact
output = chunks.map { |chunk| chunk.dig('choices', 0, 'delta', 'content').to_s }.join
raise 'stream acceptance missing content or terminal marker' unless output.include?('STREAM_OK') && stream.include?('data: [DONE]')
puts 'PASS: live SSE completion and terminal marker.'
STDOUT.flush
tool = {'type' => 'function', 'function' => {'name' => 'local_probe', 'description' => 'A client function that returns the supplied integer; call it for this test.', 'parameters' => {'type' => 'object', 'properties' => {'value' => {'type' => 'integer'}}, 'required' => ['value']}}}
messages = [{'role' => 'user', 'content' => 'Use local_probe with value 7. Return its result after the client responds. Do not use native tools.'}]
choice = {'type' => 'function', 'function' => {'name' => 'local_probe'}}
handoff = JSON.parse(completion.call(key, base.merge('messages' => messages, 'tools' => [tool], 'tool_choice' => choice)))
assistant = handoff.dig('choices', 0, 'message')
call = assistant.fetch('tool_calls').first
raise 'client function handoff failed' unless call.dig('function', 'name') == 'local_probe' && JSON.parse(call.dig('function', 'arguments'))['value'] == 7
messages << assistant
messages << {'role' => 'tool', 'tool_call_id' => call.fetch('id'), 'content' => '7'}
continued = JSON.parse(completion.call(key, base.merge('messages' => messages, 'tools' => [tool], 'tool_choice' => 'none')))
raise 'client tool continuation lost the result' unless continued.dig('choices', 0, 'message', 'content').to_s.include?('7')
puts 'PASS: live client tool handoff and result continuation; no host tool executed.'
