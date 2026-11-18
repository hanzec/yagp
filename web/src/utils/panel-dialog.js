import { ElMessageBox } from 'element-plus'
import 'element-plus/es/components/message-box/style/css'

export function confirmPanelAction(message, title, options = {}) {
  return ElMessageBox.confirm(message, title, {
    modalClass: 'panel-dialog-overlay',
    ...options,
    customClass: options.customClass ? `panel-confirm ${options.customClass}` : 'panel-confirm'
  })
}
