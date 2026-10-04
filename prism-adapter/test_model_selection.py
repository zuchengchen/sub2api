"""Selector failures must not silently retain a previous model or effort."""
import unittest
from unittest import mock

from test_server import adapter
import model_selection as selection


class SelectorTests(unittest.IsolatedAsyncioTestCase):
    async def test_catalog_failure_stops_before_selecting_or_submitting(self):
        page = mock.Mock(wait_for_function=mock.AsyncMock(), evaluate=mock.AsyncMock(return_value=False))
        with self.assertRaises(adapter.AdapterError) as raised:
            await selection.select_options_async(page, 'gpt-6.1-sol', 'medium', adapter.AdapterError)
        self.assertEqual((raised.exception.status, raised.exception.code), (503, 'model_catalog_unavailable'))
        page.get_by_role.assert_not_called()

    async def test_unavailable_model_is_rejected_without_effort_fallback(self):
        trigger = mock.Mock(wait_for=mock.AsyncMock(), inner_text=mock.AsyncMock(return_value='5.6 Sol\nMedium'),
                            click=mock.AsyncMock())
        menu = mock.Mock(hover=mock.AsyncMock())
        unavailable = mock.Mock(click=mock.AsyncMock(side_effect=TimeoutError('fixture')))
        page = mock.Mock(wait_for_function=mock.AsyncMock(), evaluate=mock.AsyncMock(return_value=True))
        page.get_by_role.side_effect = [trigger, menu, unavailable]
        with self.assertRaises(adapter.AdapterError) as raised:
            await selection.select_options_async(page, 'gpt-6.1-sol', 'xhigh', adapter.AdapterError)
        self.assertEqual((raised.exception.status, raised.exception.code), (422, 'model_unavailable'))
        self.assertEqual(page.get_by_role.call_count, 3)

    async def test_stale_selection_is_rejected_after_clicks(self):
        trigger = mock.Mock(wait_for=mock.AsyncMock(), inner_text=mock.AsyncMock(return_value='5.6 Sol\nMedium'),
                            click=mock.AsyncMock())
        menu = mock.Mock(hover=mock.AsyncMock())
        option = mock.Mock(click=mock.AsyncMock())
        page = mock.Mock(wait_for_function=mock.AsyncMock(), evaluate=mock.AsyncMock(return_value=True))
        page.get_by_role.side_effect = [trigger, menu, option]
        with self.assertRaises(adapter.AdapterError) as raised:
            await selection.select_options_async(page, 'gpt-6.1-sol', 'medium', adapter.AdapterError)
        self.assertEqual(raised.exception.code, 'selection_mismatch')

    def test_sync_selector_rejects_unavailable_effort(self):
        trigger = mock.Mock(inner_text=mock.Mock(return_value='6.1 Sol\nMedium'))
        menu, option = mock.Mock(), mock.Mock()
        option.click.side_effect = TimeoutError('fixture')
        page = mock.Mock(evaluate=mock.Mock(return_value=True))
        page.get_by_role.side_effect = [trigger, menu, option]
        with self.assertRaises(adapter.AdapterError) as raised:
            selection.select_options(page, 'gpt-6.1-sol', 'xhigh', adapter.AdapterError)
        self.assertEqual((raised.exception.status, raised.exception.code), (422, 'reasoning_unavailable'))
