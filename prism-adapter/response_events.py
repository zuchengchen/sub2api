"""Completed Responses items as honest, non-token-streaming SSE events."""


def completed_events(response):
    events = []
    def emit(kind, **fields):
        events.append((kind, dict(type=kind, sequence_number=len(events), **fields)))
    created = dict(response, status='in_progress', output=[])
    emit('response.created', response=created)
    emit('response.in_progress', response=created)
    for index, item in enumerate(response['output']):
        added = dict(item, status='in_progress')
        kind = item['type']
        if kind == 'message':
            added['content'] = []
        elif kind == 'function_call':
            added['arguments'] = ''
        else:
            added['input'] = ''
        emit('response.output_item.added', output_index=index, item=added)
        if kind == 'message':
            for content_index, part in enumerate(item['content']):
                emit('response.content_part.added', output_index=index, item_id=item['id'], content_index=content_index,
                     part=dict(part, text=''))
                emit('response.output_text.done', output_index=index, item_id=item['id'], content_index=content_index, text=part['text'])
                emit('response.content_part.done', output_index=index, item_id=item['id'], content_index=content_index, part=part)
        elif kind == 'function_call':
            emit('response.function_call_arguments.done', output_index=index, item_id=item['id'], arguments=item['arguments'])
        else:
            emit('response.custom_tool_call_input.done', output_index=index, item_id=item['id'], input=item['input'])
        emit('response.output_item.done', output_index=index, item=item)
    emit('response.completed', response=response)
    return events
