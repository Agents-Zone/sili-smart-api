/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import {
  ADMIN_ONLY_SECTIONS,
  type DashboardSectionId,
} from '../section-registry'

// Whether a section is restricted to admins, derived from the registry's
// adminOnly flags (see ADMIN_ONLY_SECTIONS).
export function isSectionAdminOnly(id: string): boolean {
  return ADMIN_ONLY_SECTIONS.has(id as DashboardSectionId)
}
