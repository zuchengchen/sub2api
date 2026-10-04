"""Bounded schema/grammar validation. Never executes a client tool or code."""
import json
import math
import re
import sys


def bounded(value, depth=0):
    if depth > 64:
        raise ValueError('nesting')
    if isinstance(value, dict):
        for key, child in value.items():
            if key in ('$ref', '$dynamicRef', '$recursiveRef') and (not isinstance(child, str) or not child.startswith('#')):
                raise ValueError('external reference')
            bounded(child, depth + 1)
    elif isinstance(value, list):
        for child in value:
            bounded(child, depth + 1)
    elif isinstance(value, float) and not math.isfinite(value):
        raise ValueError('nonfinite')


def grammar(format_, value=None):
    if format_.get('type') == 'text':
        return
    if format_.get('type') != 'grammar' or not isinstance(format_.get('definition'), str):
        raise ValueError('format')
    definition = format_['definition']
    if not definition or len(definition) > 16000:
        raise ValueError('grammar size')
    if format_.get('syntax') == 'regex':
        compiled = re.compile(definition)
        if value is not None and compiled.fullmatch(value) is None:
            raise ValueError('grammar mismatch')
    elif format_.get('syntax') == 'lark':
        # Only Lark's bundled common terminals may be imported. Never resolve
        # a caller-selected local grammar file or package.
        for imported in re.findall(r'%import\s+([^\s(]+)', definition):
            if imported != 'common' and not re.fullmatch(r'common\.[A-Z][A-Z0-9_]*', imported):
                raise ValueError('external grammar')
        from lark import Lark
        compiled = Lark(definition, parser='earley', lexer='dynamic', start='start')
        if value is not None:
            compiled.parse(value)
    else:
        raise ValueError('grammar syntax')


def validate(commands):
    from jsonschema import Draft202012Validator
    from referencing import Registry
    from referencing.exceptions import NoSuchResource

    def unavailable(uri):
        raise NoSuchResource(ref=uri)

    if not isinstance(commands, list) or len(commands) > 192:
        raise ValueError('command budget')
    for command in commands:
        if command['kind'] == 'function':
            schema = command['parameters']
            bounded(schema)
            if schema.get('$schema', 'https://json-schema.org/draft/2020-12/schema') not in (
                    'https://json-schema.org/draft/2020-12/schema', 'http://json-schema.org/draft-07/schema#'):
                raise ValueError('schema draft')
            from jsonschema import Draft7Validator
            cls = Draft7Validator if 'draft-07' in schema.get('$schema', '') else Draft202012Validator
            cls.check_schema(schema)
            if 'value' in command:
                bounded(command['value'])
                cls(schema, registry=Registry(retrieve=unavailable)).validate(command['value'])
        elif command['kind'] == 'custom':
            grammar(command['format'], command.get('value'))
        else:
            raise ValueError('tool kind')


if __name__ == '__main__':
    try:
        import resource
        resource.setrlimit(resource.RLIMIT_CPU, (2, 2))
        if sys.platform.startswith('linux'):
            resource.setrlimit(resource.RLIMIT_AS, (192 << 20, 192 << 20))
        raw = sys.stdin.buffer.read((1 << 20) + 1)
        if len(raw) > 1 << 20:
            raise ValueError('byte budget')
        validate(json.loads(raw))
    except BaseException:
        print('invalid')
        sys.exit(1)
    print('valid')
